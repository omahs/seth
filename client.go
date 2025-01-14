package seth

import (
	"context"
	"crypto/ecdsa"
	verr "errors"
	"fmt"
	"math/big"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/avast/retry-go"
	"github.com/ethereum/go-ethereum"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
)

const (
	ErrEmptyConfigPath      = "toml config path is empty, set SETH_CONFIG_PATH"
	ErrCreateABIStore       = "failed to create ABI store"
	ErrReadingKeys          = "failed to read keys"
	ErrCreateNonceManager   = "failed to create nonce manager"
	ErrCreateTracer         = "failed to create tracer"
	ErrReadContractMap      = "failed to read deployed contract map"
	ErrNoKeyLoaded          = "failed to load private key"
	ErrRpcHealthCheckFailed = "RPC health check failed ¯\\_(ツ)_/¯"

	ContractMapFilePattern          = "deployed_contracts_%s_%s.toml"
	RevertedTransactionsFilePattern = "reverted_transactions_%s_%s.json"
)

var (
	// Amount of funds that will be left on the root key, when splitting funds between ephemeral addresses
	ZeroInt64 int64 = 0

	TracingLevel_None     = "NONE"
	TracingLevel_Reverted = "REVERTED"
	TracingLevel_All      = "ALL"
)

// Client is a vanilla go-ethereum client with enhanced debug logging
type Client struct {
	Cfg                      *Config
	Client                   *ethclient.Client
	Addresses                []common.Address
	PrivateKeys              []*ecdsa.PrivateKey
	ChainID                  int64
	URL                      string
	Context                  context.Context
	CancelFunc               context.CancelFunc
	Errors                   []error
	ContractStore            *ContractStore
	NonceManager             *NonceManager
	Tracer                   *Tracer
	ContractAddressToNameMap ContractMap
	ABIFinder                *ABIFinder
	HeaderCache              *LFUHeaderCache
}

// NewClientWithConfig creates a new seth client with all deps setup from config
func NewClientWithConfig(cfg *Config) (*Client, error) {
	initDefaultLogging()

	err := ValidateConfig(cfg)
	if err != nil {
		return nil, err
	}

	L.Debug().Msgf("Using tracing level: %s", cfg.TracingLevel)

	cfg.setEphemeralAddrs()
	cs, err := NewContractStore(filepath.Join(cfg.ConfigDir, cfg.ABIDir), filepath.Join(cfg.ConfigDir, cfg.BINDir))
	if err != nil {
		return nil, errors.Wrap(err, ErrCreateABIStore)
	}
	if cfg.ephemeral {
		// we don't care about any other keys, only the root key
		// you should not use ephemeral mode with more than 1 key
		if len(cfg.Network.PrivateKeys) > 1 {
			L.Warn().Msg("Ephemeral mode is enabled, but more than 1 key is loaded. Only the first key will be used")
		}
		cfg.Network.PrivateKeys = cfg.Network.PrivateKeys[:1]
		pkeys, err := NewEphemeralKeys(*cfg.EphemeralAddrs)
		if err != nil {
			return nil, err
		}
		cfg.Network.PrivateKeys = append(cfg.Network.PrivateKeys, pkeys...)
	} else {
		if err := readKeyFileConfig(cfg); err != nil {
			return nil, err
		}
	}
	addrs, pkeys, err := cfg.ParseKeys()
	if err != nil {
		return nil, errors.Wrap(err, ErrReadingKeys)
	}
	nm, err := NewNonceManager(cfg, addrs, pkeys)
	if err != nil {
		return nil, errors.Wrap(err, ErrCreateNonceManager)
	}

	if !cfg.IsSimulatedNetwork() && cfg.SaveDeployedContractsMap && cfg.ContractMapFile == "" {
		cfg.ContractMapFile = cfg.GenerateContractMapFileName()
	}

	// this part is kind of duplicated in NewClientRaw, but we need to create contract map before creating Tracer
	// so that both the tracer and client have references to the same map
	contractAddressToNameMap := NewEmptyContractMap()
	contractAddressToNameMap.addressMap = make(map[string]string)
	if !cfg.IsSimulatedNetwork() {
		contractAddressToNameMap.addressMap, err = LoadDeployedContracts(cfg.ContractMapFile)
		if err != nil {
			return nil, errors.Wrap(err, ErrReadContractMap)
		}
	} else {
		L.Debug().Msg("Simulated network, contract map won't be read from file")
	}

	abiFinder := NewABIFinder(contractAddressToNameMap, cs)
	if len(cfg.Network.URLs) == 0 {
		return nil, fmt.Errorf("at least one url should be present in config in 'secret_urls = []'")
	}
	tr, err := NewTracer(cfg.Network.URLs[0], cs, &abiFinder, cfg, contractAddressToNameMap, addrs)
	if err != nil {
		return nil, errors.Wrap(err, ErrCreateTracer)
	}

	return NewClientRaw(
		cfg,
		addrs,
		pkeys,
		WithContractStore(cs),
		WithNonceManager(nm),
		WithTracer(tr),
		WithContractMap(contractAddressToNameMap),
		WithABIFinder(&abiFinder),
	)
}

func ValidateConfig(cfg *Config) error {
	if cfg.Network.GasPriceEstimationEnabled {
		if cfg.Network.GasPriceEstimationBlocks == 0 {
			return errors.New("when automating gas estimation is enabled blocks must be greater than 0. fix it or disable gas estimation")
		}
		cfg.Network.GasPriceEstimationTxPriority = strings.ToLower(cfg.Network.GasPriceEstimationTxPriority)

		if cfg.Network.GasPriceEstimationTxPriority == "" {
			cfg.Network.GasPriceEstimationTxPriority = Priority_Standard
		}

		switch cfg.Network.GasPriceEstimationTxPriority {
		case Priority_Degen:
		case Priority_Fast:
		case Priority_Standard:
		case Priority_Slow:
		default:
			return errors.New("when automating gas estimation is enabled priority must be fast, standard or slow. fix it or disable gas estimation")
		}

	}

	if cfg.Network.GasLimit != 0 {
		L.Warn().
			Msg("Gas limit is set, this will override the gas limit set by the network. This option should be used **ONLY** if node is incapable of estimating gas limit itself, which happens only with very old versions")
	}

	if cfg.TracingLevel == "" {
		cfg.TracingLevel = TracingLevel_Reverted
	}

	cfg.TracingLevel = strings.ToUpper(cfg.TracingLevel)

	switch cfg.TracingLevel {
	case TracingLevel_None:
	case TracingLevel_Reverted:
	case TracingLevel_All:
	default:
		return errors.New("tracing level must be one of: NONE, REVERTED, ALL")
	}

	if cfg.KeyFileSource != "" && cfg.EphemeralAddrs != nil && *cfg.EphemeralAddrs != 0 {
		return fmt.Errorf("KeyFileSource is set to '%s' and ephemeral addresses are enabled, please disable ephemeral addresses or the keyfile usage. You cannot use both modes at the same time", cfg.KeyFileSource)
	}

	switch cfg.KeyFileSource {
	case "", KeyFileSourceFile, KeyFileSourceBase64EnvVar:
	default:
		return fmt.Errorf("KeyFileSource must be either empty (disabled) or one of: '%s', '%s'", KeyFileSourceFile, KeyFileSourceBase64EnvVar)
	}

	if cfg.KeyFileSource == KeyFileSourceFile && cfg.KeyFilePath == "" {
		return fmt.Errorf("KeyFileSource is set to 'file' but the path to the key file is not set")
	}

	return nil
}

// NewClient creates a new raw seth client with all deps setup from env vars
func NewClient() (*Client, error) {
	cfg, err := ReadConfig()
	if err != nil {
		return nil, err
	}
	return NewClientWithConfig(cfg)
}

// NewClientRaw creates a new raw seth client without dependencies
func NewClientRaw(
	cfg *Config,
	addrs []common.Address,
	pkeys []*ecdsa.PrivateKey,
	opts ...ClientOpt,
) (*Client, error) {
	if len(cfg.Network.URLs) == 0 {
		return nil, errors.New("no RPC URL provided")
	}
	if len(cfg.Network.URLs) > 1 {
		L.Warn().Msg("Multiple RPC URLs provided, only the first one will be used")
	}

	client, err := ethclient.Dial(cfg.Network.URLs[0])
	if err != nil {
		return nil, fmt.Errorf("failed to connect to '%s' due to: %w", cfg.Network.URLs[0], err)
	}

	chainId, err := client.ChainID(context.Background())
	if err != nil {
		return nil, errors.Wrap(err, "failed to get chain ID")
	}
	cfg.Network.ChainID = chainId.String()
	cID, err := strconv.Atoi(cfg.Network.ChainID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		Cfg:         cfg,
		Client:      client,
		Addresses:   addrs,
		PrivateKeys: pkeys,
		URL:         cfg.Network.URLs[0],
		ChainID:     int64(cID),
		Context:     ctx,
		CancelFunc:  cancel,
	}
	for _, o := range opts {
		o(c)
	}

	if c.ContractAddressToNameMap.addressMap == nil {
		c.ContractAddressToNameMap = NewEmptyContractMap()
		if !cfg.IsSimulatedNetwork() {
			c.ContractAddressToNameMap.addressMap, err = LoadDeployedContracts(cfg.ContractMapFile)
			if err != nil {
				return nil, errors.Wrap(err, ErrReadContractMap)
			}
			if len(c.ContractAddressToNameMap.addressMap) > 0 {
				L.Info().
					Int("Size", len(c.ContractAddressToNameMap.addressMap)).
					Str("File name", cfg.ContractMapFile).
					Msg("No contract map provided, read it from file")
			} else {
				L.Info().
					Msg("No contract map provided and no file found, created new one")
			}
		} else {
			L.Debug().Msg("Simulated network, contract map won't be read from file")
			L.Info().
				Msg("No contract map provided and no file found, created new one")
		}
	} else {
		L.Info().
			Int("Size", len(c.ContractAddressToNameMap.addressMap)).
			Msg("Contract map was provided")
	}
	if c.NonceManager != nil {
		c.NonceManager.Client = c
		if len(c.Cfg.Network.PrivateKeys) > 0 {
			if err := c.NonceManager.UpdateNonces(); err != nil {
				return nil, err
			}
		}
	}

	if cfg.CheckRpcHealthOnStart {
		if c.NonceManager == nil {
			L.Warn().Msg("Nonce manager is not set, RPC health check will be skipped. Client will most probably fail on first transaction")
		} else {
			if err := c.checkRPCHealth(); err != nil {
				return nil, err
			}
		}
	}

	cfg.setEphemeralAddrs()

	L.Info().
		Str("NetworkName", cfg.Network.Name).
		Interface("Addresses", addrs).
		Str("RPC", cfg.Network.URLs[0]).
		Str("ChainID", cfg.Network.ChainID).
		Int64("Ephemeral keys", *cfg.EphemeralAddrs).
		Msg("Created new client")

	if cfg.ephemeral {
		gasPrice, err := c.GetSuggestedLegacyFees(context.Background(), Priority_Standard)
		if err != nil {
			gasPrice = big.NewInt(c.Cfg.Network.GasPrice)
		}

		bd, err := c.CalculateSubKeyFunding(*cfg.EphemeralAddrs, gasPrice.Int64(), *cfg.RootKeyFundsBuffer)
		if err != nil {
			return nil, err
		}
		L.Warn().Msg("Ephemeral mode, all funds will be lost!")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		eg, egCtx := errgroup.WithContext(ctx)
		// root key is element 0 in ephemeral
		for _, addr := range c.Addresses[1:] {
			addr := addr
			eg.Go(func() error {
				return c.TransferETHFromKey(egCtx, 0, addr.Hex(), bd.AddrFunding, gasPrice)
			})
		}
		if err := eg.Wait(); err != nil {
			return nil, err
		}
	}

	if c.Cfg.TracingLevel != TracingLevel_None && c.Tracer == nil {
		if c.ContractStore == nil {
			cs, err := NewContractStore(filepath.Join(cfg.ConfigDir, cfg.ABIDir), filepath.Join(cfg.ConfigDir, cfg.BINDir))
			if err != nil {
				return nil, errors.Wrap(err, ErrCreateABIStore)
			}
			c.ContractStore = cs
		}
		if c.ABIFinder == nil {
			abiFinder := NewABIFinder(c.ContractAddressToNameMap, c.ContractStore)
			c.ABIFinder = &abiFinder
		}
		tr, err := NewTracer(cfg.Network.URLs[0], c.ContractStore, c.ABIFinder, cfg, c.ContractAddressToNameMap, addrs)
		if err != nil {
			return nil, errors.Wrap(err, ErrCreateTracer)
		}

		c.Tracer = tr
	}

	now := time.Now().Format("2006-01-02-15-04-05")
	c.Cfg.RevertedTransactionsFile = fmt.Sprintf(RevertedTransactionsFilePattern, c.Cfg.Network.Name, now)

	if c.Cfg.Network.GasPriceEstimationEnabled {
		L.Debug().Msg("Gas estimation is enabled")
		L.Debug().Msg("Initialising LFU block header cache")
		c.HeaderCache = NewLFUBlockCache(c.Cfg.Network.GasPriceEstimationBlocks)

		if c.Cfg.Network.EIP1559DynamicFees {
			L.Debug().Msg("Checking if EIP-1559 is supported by the network")
			c.CalculateGasEstimations(GasEstimationRequest{
				GasEstimationEnabled: true,
				FallbackGasPrice:     c.Cfg.Network.GasPrice,
				FallbackGasFeeCap:    c.Cfg.Network.GasFeeCap,
				FallbackGasTipCap:    c.Cfg.Network.GasTipCap,
				Priority:             Priority_Standard,
			})
		}
	}

	return c, nil
}

func (m *Client) checkRPCHealth() error {
	L.Info().Str("RPC node", m.URL).Msg("---------------- !!!!! ----------------> Checking RPC health")
	ctx, cancel := context.WithTimeout(context.Background(), m.Cfg.Network.TxnTimeout.Duration())
	defer cancel()

	gasPrice, err := m.GetSuggestedLegacyFees(context.Background(), Priority_Standard)
	if err != nil {
		gasPrice = big.NewInt(m.Cfg.Network.GasPrice)
	}

	err = m.TransferETHFromKey(ctx, 0, m.Addresses[0].Hex(), big.NewInt(10_000), gasPrice)
	if err != nil {
		return errors.Wrap(err, ErrRpcHealthCheckFailed)
	}

	L.Info().Msg("RPC health check passed <---------------- !!!!! ----------------")
	return nil
}

// Decode waits for transaction to be minted, then decodes transaction inputs, outputs, logs and events and
// depending on 'tracing_level' it either returns immediatelly or if the level matches it traces all calls.
// If 'tracing_to_json' is saved we also save to JSON all that information.
// If transaction was reverted the error return will be revert error, not decoding error (that one if any will be logged).
// It means it can return both error and decoded transaction!
func (m *Client) Decode(tx *types.Transaction, txErr error) (*DecodedTransaction, error) {
	if len(m.Errors) > 0 {
		return nil, verr.Join(m.Errors...)
	}
	if txErr != nil {
		//try to decode revert reason
		reason, decodingErr := m.DecodeCustomABIErr(txErr)

		if decodingErr == nil {
			return nil, errors.Wrap(txErr, reason)
		}

		L.Trace().
			Msg("Skipping decoding, transaction submission failed. Nothing to decode")
		return nil, txErr
	}

	if tx == nil {
		L.Trace().
			Msg("Skipping decoding, because transaction is nil. Nothing to decode")
		return nil, nil
	}

	l := L.With().Str("Transaction", tx.Hash().Hex()).Logger()
	receipt, err := m.WaitMined(context.Background(), l, m.Client, tx)
	if err != nil {
		L.Trace().
			Err(err).
			Msg("Skipping decoding, because transaction was not minted. Nothing to decode")
		return nil, err
	}

	var revertErr error
	if receipt.Status == 0 {
		revertErr = m.callAndGetRevertReason(tx, receipt)
	}

	decoded, decodeErr := m.decodeTransaction(l, tx, receipt)

	if decodeErr != nil && errors.Is(decodeErr, errors.New(ErrNoABIMethod)) {
		if m.Cfg.TraceToJson {
			L.Trace().
				Err(decodeErr).
				Msg("Failed to decode transaction. Saving transaction data hash as JSON")

			err = CreateOrAppendToJsonArray(m.Cfg.RevertedTransactionsFile, tx.Hash().Hex())
			if err != nil {
				l.Warn().
					Err(err).
					Str("TXHash", tx.Hash().Hex()).
					Msg("Failed to save reverted transaction hash to file")
			} else {
				l.Trace().
					Str("TXHash", tx.Hash().Hex()).
					Msg("Saved reverted transaction to file")
			}
		}
		return decoded, revertErr
	}

	if m.Cfg.TracingLevel == TracingLevel_None {
		L.Trace().
			Str("Transaction Hash", tx.Hash().Hex()).
			Msg("Tracing level is NONE, skipping decoding")
		return decoded, revertErr
	}

	if m.Cfg.TracingLevel == TracingLevel_All || (m.Cfg.TracingLevel == TracingLevel_Reverted && revertErr != nil) {
		traceErr := m.Tracer.TraceGethTX(decoded.Hash)
		if traceErr != nil {
			if m.Cfg.TraceToJson {
				L.Trace().
					Err(traceErr).
					Msg("Failed to trace call, but decoding was successful. Saving decoded data as JSON")

				path, saveErr := saveAsJson(decoded, "traces", decoded.Hash)
				if saveErr != nil {
					L.Warn().
						Err(saveErr).
						Msg("Failed to save decoded call as JSON")
				} else {
					L.Trace().
						Str("Path", path).
						Str("Tx hash", decoded.Hash).
						Msg("Saved decoded transaction data to JSON")
				}
			}

			if strings.Contains(traceErr.Error(), "debug_traceTransaction does not exist") {
				L.Warn().
					Err(err).
					Msg("Debug API is either disabled or not available on the node. Disabling tracing")

				m.Cfg.TracingLevel = TracingLevel_None
			}

			return decoded, revertErr
		}

		if m.Cfg.TraceToJson {
			path, saveErr := saveAsJson(m.Tracer.DecodedCalls[decoded.Hash], "traces", decoded.Hash)
			if saveErr != nil {
				L.Warn().
					Err(saveErr).
					Msg("Failed to save decoded call as JSON")
			} else {
				L.Trace().
					Str("Path", path).
					Str("Tx hash", decoded.Hash).
					Msg("Saved decoded call data to JSON")
			}
		}
	} else {
		L.Trace().
			Str("Transaction Hash", tx.Hash().Hex()).
			Str("Tracing level", m.Cfg.TracingLevel).
			Bool("Was reverted?", revertErr != nil).
			Msg("Transaction doesn't match tracing level, skipping decoding")
	}

	return decoded, revertErr
}

func (m *Client) TransferETHFromKey(ctx context.Context, fromKeyNum int, to string, value *big.Int, gasPrice *big.Int) error {
	if fromKeyNum > len(m.PrivateKeys) || fromKeyNum > len(m.Addresses) {
		return errors.Wrap(errors.New(ErrNoKeyLoaded), fmt.Sprintf("requested key: %d", fromKeyNum))
	}
	toAddr := common.HexToAddress(to)
	chainID, err := m.Client.NetworkID(context.Background())
	if err != nil {
		return errors.Wrap(err, "failed to get network ID")
	}

	var gasLimit int64
	gasLimitRaw, err := m.EstimateGasLimitForFundTransfer(m.Addresses[fromKeyNum], common.HexToAddress(to), value)
	if err != nil {
		gasLimit = m.Cfg.Network.TransferGasFee
	} else {
		gasLimit = int64(gasLimitRaw)
	}

	if gasPrice == nil {
		gasPrice = big.NewInt(m.Cfg.Network.GasPrice)
	}

	rawTx := &types.LegacyTx{
		Nonce:    m.NonceManager.NextNonce(m.Addresses[fromKeyNum]).Uint64(),
		To:       &toAddr,
		Value:    value,
		Gas:      uint64(gasLimit),
		GasPrice: gasPrice,
	}
	L.Debug().Interface("TransferTx", rawTx).Send()
	signedTx, err := types.SignNewTx(m.PrivateKeys[fromKeyNum], types.NewEIP155Signer(chainID), rawTx)
	if err != nil {
		return errors.Wrap(err, "failed to sign tx")
	}

	ctx, cancel := context.WithTimeout(ctx, m.Cfg.Network.TxnTimeout.Duration())
	defer cancel()
	err = m.Client.SendTransaction(ctx, signedTx)
	if err != nil {
		return errors.Wrap(err, "failed to send transaction")
	}
	l := L.With().Str("Transaction", signedTx.Hash().Hex()).Logger()
	l.Info().
		Int("FromKeyNum", fromKeyNum).
		Str("To", to).
		Interface("Value", value).
		Msg("Send ETH")
	_, err = m.WaitMined(ctx, l, m.Client, signedTx)
	if err != nil {
		return err
	}
	return err
}

// WaitMined the same as bind.WaitMined, awaits transaction receipt until timeout
func (m *Client) WaitMined(ctx context.Context, l zerolog.Logger, b bind.DeployBackend, tx *types.Transaction) (*types.Receipt, error) {
	queryTicker := time.NewTicker(time.Second)
	defer queryTicker.Stop()
	ctx, cancel := context.WithTimeout(ctx, m.Cfg.Network.TxnTimeout.Duration())
	defer cancel()
	for {
		receipt, err := b.TransactionReceipt(ctx, tx.Hash())
		if err == nil {
			l.Info().
				Int64("BlockNumber", receipt.BlockNumber.Int64()).
				Str("TX", tx.Hash().String()).
				Msg("Transaction accepted")
			return receipt, nil
		}
		if errors.Is(err, ethereum.NotFound) {
			l.Debug().
				Str("TX", tx.Hash().String()).
				Msg("Awaiting transaction")
		} else {
			l.Warn().
				Err(err).
				Str("TX", tx.Hash().String()).
				Msg("Failed to get receipt")
		}
		select {
		case <-ctx.Done():
			l.Error().Err(err).Msg("Transaction context is done")
			return nil, ctx.Err()
		case <-queryTicker.C:
		}
	}
}

/* ClientOpts client functional options */

// ClientOpt is a client functional option
type ClientOpt func(c *Client)

// WithContractStore ContractStore functional option
func WithContractStore(as *ContractStore) ClientOpt {
	return func(c *Client) {
		c.ContractStore = as
	}
}

// WithContractMap contractAddressToNameMap functional option
func WithContractMap(contractAddressToNameMap ContractMap) ClientOpt {
	return func(c *Client) {
		c.ContractAddressToNameMap = contractAddressToNameMap
	}
}

// WithABIFinder ABIFinder functional option
func WithABIFinder(abiFinder *ABIFinder) ClientOpt {
	return func(c *Client) {
		c.ABIFinder = abiFinder
	}
}

// WithNonceManager NonceManager functional option
func WithNonceManager(nm *NonceManager) ClientOpt {
	return func(c *Client) {
		c.NonceManager = nm
	}
}

// WithTracer Tracer functional option
func WithTracer(t *Tracer) ClientOpt {
	return func(c *Client) {
		c.Tracer = t
	}
}

/* CallOpts function options */

// CallOpt is a functional option for bind.CallOpts
type CallOpt func(o *bind.CallOpts)

// WithPending sets pending option for bind.CallOpts
func WithPending(pending bool) CallOpt {
	return func(o *bind.CallOpts) {
		o.Pending = pending
	}
}

// WithBlockNumber sets blockNumber option for bind.CallOpts
func WithBlockNumber(bn uint64) CallOpt {
	return func(o *bind.CallOpts) {
		o.BlockNumber = big.NewInt(int64(bn))
	}
}

// NewCallOpts returns a new sequential call options wrapper
func (m *Client) NewCallOpts(o ...CallOpt) *bind.CallOpts {
	co := &bind.CallOpts{
		Pending: false,
		From:    m.Addresses[0],
	}
	for _, f := range o {
		f(co)
	}
	return co
}

// NewCallKeyOpts returns a new sequential call options wrapper from the key N
func (m *Client) NewCallKeyOpts(keyNum int, o ...CallOpt) *bind.CallOpts {
	co := &bind.CallOpts{
		Pending: false,
		From:    m.Addresses[keyNum],
	}
	for _, f := range o {
		f(co)
	}
	return co
}

// TransactOpt is a wrapper for bind.TransactOpts
type TransactOpt func(o *bind.TransactOpts)

// WithValue sets value option for bind.TransactOpts
func WithValue(value *big.Int) TransactOpt {
	return func(o *bind.TransactOpts) {
		o.Value = value
	}
}

// WithGasPrice sets gasPrice option for bind.TransactOpts
func WithGasPrice(gasPrice *big.Int) TransactOpt {
	return func(o *bind.TransactOpts) {
		o.GasPrice = gasPrice
	}
}

// WithGasLimit sets gasLimit option for bind.TransactOpts
func WithGasLimit(gasLimit uint64) TransactOpt {
	return func(o *bind.TransactOpts) {
		o.GasLimit = gasLimit
	}
}

// WithNoSend sets noSend option for bind.TransactOpts
func WithNoSend(noSend bool) TransactOpt {
	return func(o *bind.TransactOpts) {
		o.NoSend = noSend
	}
}

// WithNonce sets nonce option for bind.TransactOpts
func WithNonce(nonce *big.Int) TransactOpt {
	return func(o *bind.TransactOpts) {
		o.Nonce = nonce
	}
}

// WithGasFeeCap sets gasFeeCap option for bind.TransactOpts
func WithGasFeeCap(gasFeeCap *big.Int) TransactOpt {
	return func(o *bind.TransactOpts) {
		o.GasFeeCap = gasFeeCap
	}
}

// WithGasTipCap sets gasTipCap option for bind.TransactOpts
func WithGasTipCap(gasTipCap *big.Int) TransactOpt {
	return func(o *bind.TransactOpts) {
		o.GasTipCap = gasTipCap
	}
}

type ContextErrorKey struct{}

// NewTXOpts returns a new transaction options wrapper,
// Sets gas price/fee tip/cap and gas limit either based on TOML config or estimations.
func (m *Client) NewTXOpts(o ...TransactOpt) *bind.TransactOpts {
	opts, nonce, estimations := m.getProposedTransactionOptions(0)
	m.configureTransactionOpts(opts, nonce.PendingNonce, estimations, o...)
	L.Debug().
		Interface("Nonce", opts.Nonce).
		Interface("Value", opts.Value).
		Interface("GasPrice", opts.GasPrice).
		Interface("GasFeeCap", opts.GasFeeCap).
		Interface("GasTipCap", opts.GasTipCap).
		Uint64("GasLimit", opts.GasLimit).
		Msg("New transaction options")
	return opts
}

// NewTXKeyOpts returns a new transaction options wrapper,
// sets opts.GasPrice and opts.GasLimit from seth.toml or override with options
func (m *Client) NewTXKeyOpts(keyNum int, o ...TransactOpt) *bind.TransactOpts {
	if keyNum > len(m.Addresses) || keyNum < 0 {
		errText := fmt.Sprintf("keyNum is out of range. Expected %d-%d. Got: %d", 0, len(m.Addresses)-1, keyNum)
		if keyNum == TimeoutKeyNum {
			errText += " (this is a probably because, we didn't manage to find any synced key before timeout)"
		}

		err := errors.New(errText)
		m.Errors = append(m.Errors, err)
		opts := &bind.TransactOpts{}

		// can't return nil, otherwise RPC wrapper will panic and we might lose funds on testnets/mainnets, that's why
		// error is passed in Context here to avoid panic, whoever is using Seth should make sure that there is no error
		// present in Context before using *bind.TransactOpts
		opts.Context = context.WithValue(context.Background(), ContextErrorKey{}, err)

		return opts
	}
	L.Debug().
		Interface("KeyNum", keyNum).
		Interface("Address", m.Addresses[keyNum]).
		Msg("Estimating transaction")
	opts, nonceStatus, estimations := m.getProposedTransactionOptions(keyNum)

	m.configureTransactionOpts(opts, nonceStatus.PendingNonce, estimations, o...)
	L.Debug().
		Interface("KeyNum", keyNum).
		Interface("Nonce", opts.Nonce).
		Interface("Value", opts.Value).
		Interface("GasPrice", opts.GasPrice).
		Interface("GasFeeCap", opts.GasFeeCap).
		Interface("GasTipCap", opts.GasTipCap).
		Uint64("GasLimit", opts.GasLimit).
		Msg("New transaction options")
	return opts
}

// AnySyncedKey returns the first synced key
func (m *Client) AnySyncedKey() int {
	return m.NonceManager.anySyncedKey()
}

type GasEstimations struct {
	GasPrice  *big.Int
	GasTipCap *big.Int
	GasFeeCap *big.Int
}

type NonceStatus struct {
	LastNonce    uint64
	PendingNonce uint64
}

func (m *Client) getNonceStatus(keyNum int) (NonceStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), m.Cfg.Network.TxnTimeout.Duration())
	defer cancel()
	pendingNonce, err := m.Client.PendingNonceAt(ctx, m.Addresses[keyNum])
	if err != nil {
		L.Error().Err(err).Msg("Failed to get pending nonce")
		return NonceStatus{}, err
	}

	lastNonce, err := m.Client.NonceAt(ctx, m.Addresses[keyNum], nil)
	if err != nil {
		return NonceStatus{}, err
	}

	return NonceStatus{
		LastNonce:    lastNonce,
		PendingNonce: pendingNonce,
	}, nil
}

// getProposedTransactionOptions gets all the tx info that network proposed
func (m *Client) getProposedTransactionOptions(keyNum int) (*bind.TransactOpts, NonceStatus, GasEstimations) {
	nonceStatus, err := m.getNonceStatus(keyNum)
	if err != nil {
		m.Errors = append(m.Errors, err)
		// can't return nil, otherwise RPC wrapper will panic
		ctx := context.WithValue(context.Background(), ContextErrorKey{}, err)

		return &bind.TransactOpts{Context: ctx}, NonceStatus{}, GasEstimations{}
	}

	var ctx context.Context

	if m.Cfg.PendingNonceProtectionEnabled {
		if nonceStatus.PendingNonce > nonceStatus.LastNonce {
			errMsg := `
pending nonce for key %d is higher than last nonce, there are %d pending transactions.

This issue is caused by one of two things:
1. You are using the same keyNum in multiple goroutines, which is not supported. Each goroutine should use an unique keyNum.
2. You have stuck transaction(s). Speed them up by sending replacement transactions with higher gas price before continuing, otherwise future transactions most probably will also get stuck.
`
			err := fmt.Errorf(errMsg, keyNum, nonceStatus.PendingNonce-nonceStatus.LastNonce)
			m.Errors = append(m.Errors, err)
			// can't return nil, otherwise RPC wrapper will panic and we might lose funds on testnets/mainnets, that's why
			// error is passed in Context here to avoid panic, whoever is using Seth should make sure that there is no error
			// present in Context before using *bind.TransactOpts
			ctx = context.WithValue(context.Background(), ContextErrorKey{}, err)
		}
		L.Debug().
			Msg("Pending nonce protection is enabled. Nonce status is OK")
	}

	estimations := m.CalculateGasEstimations(m.NewDefaultGasEstimationRequest())

	L.Debug().
		Interface("KeyNum", keyNum).
		Uint64("Nonce", nonceStatus.PendingNonce).
		Interface("GasEstimations", estimations).
		Msg("Proposed transaction options")

	opts, err := bind.NewKeyedTransactorWithChainID(m.PrivateKeys[keyNum], big.NewInt(m.ChainID))
	if err != nil {
		err = errors.Wrapf(err, "failed to create transactor for key %d", keyNum)
		m.Errors = append(m.Errors, err)
		// can't return nil, otherwise RPC wrapper will panic and we might lose funds on testnets/mainnets, that's why
		// error is passed in Context here to avoid panic, whoever is using Seth should make sure that there is no error
		// present in Context before using *bind.TransactOpts
		ctx := context.WithValue(context.Background(), ContextErrorKey{}, err)

		return &bind.TransactOpts{Context: ctx}, NonceStatus{}, GasEstimations{}
	}

	if ctx != nil {
		opts.Context = ctx
	}

	return opts, nonceStatus, estimations
}

type GasEstimationRequest struct {
	GasEstimationEnabled bool
	FallbackGasPrice     int64
	FallbackGasFeeCap    int64
	FallbackGasTipCap    int64
	Priority             string
}

// NewDefaultGasEstimationRequest creates a new default gas estimation request based on current network configuration
func (m *Client) NewDefaultGasEstimationRequest() GasEstimationRequest {
	return GasEstimationRequest{
		GasEstimationEnabled: m.Cfg.Network.GasPriceEstimationEnabled,
		FallbackGasPrice:     m.Cfg.Network.GasPrice,
		FallbackGasFeeCap:    m.Cfg.Network.GasFeeCap,
		FallbackGasTipCap:    m.Cfg.Network.GasTipCap,
		Priority:             m.Cfg.Network.GasPriceEstimationTxPriority,
	}
}

// CalculateGasEstimations calculates gas estimations (price, tip/cap) or uses hardcoded values if estimation is disabled,
// estimation errors or network is a simulated one.
func (m *Client) CalculateGasEstimations(request GasEstimationRequest) GasEstimations {
	estimations := GasEstimations{}

	if m.Cfg.IsSimulatedNetwork() || !request.GasEstimationEnabled {
		estimations.GasPrice = big.NewInt(request.FallbackGasPrice)
		estimations.GasFeeCap = big.NewInt(request.FallbackGasFeeCap)
		estimations.GasTipCap = big.NewInt(request.FallbackGasTipCap)

		return estimations
	}

	ctx, cancel := context.WithTimeout(context.Background(), m.Cfg.Network.TxnTimeout.Duration())
	defer cancel()

	var disableEstimationsIfNeeded = func(err error) {
		if strings.Contains(err.Error(), ZeroGasSuggestedErr) {
			L.Warn().Msg("Received incorrect gas estimations. Disabling them and reverting to hardcoded values. Remember to update your config!")
			m.Cfg.Network.GasPriceEstimationEnabled = false
		}
	}

	var calculateLegacyFees = func() {
		gasPrice, err := m.GetSuggestedLegacyFees(ctx, request.Priority)
		if err != nil {
			disableEstimationsIfNeeded(err)
			L.Warn().Err(err).Msg("Failed to get suggested Legacy fees. Using hardcoded values")
			estimations.GasPrice = big.NewInt(request.FallbackGasPrice)
		} else {
			estimations.GasPrice = gasPrice
		}
	}

	if m.Cfg.Network.EIP1559DynamicFees {
		maxFee, priorityFee, err := m.GetSuggestedEIP1559Fees(ctx, request.Priority)
		if err != nil {
			L.Warn().Err(err).Msg("Failed to get suggested EIP1559 fees. Using hardcoded values")
			estimations.GasFeeCap = big.NewInt(request.FallbackGasFeeCap)
			estimations.GasTipCap = big.NewInt(request.FallbackGasTipCap)

			disableEstimationsIfNeeded(err)

			if strings.Contains(err.Error(), "method eth_maxPriorityFeePerGas") || strings.Contains(err.Error(), "method eth_maxFeePerGas") || strings.Contains(err.Error(), "method eth_feeHistory") || strings.Contains(err.Error(), "expected input list for types.txdata") {
				L.Warn().Msg("EIP1559 fees are not supported by the network. Switching to Legacy fees. Remember to update your config!")
				if m.Cfg.Network.GasPrice == 0 {
					L.Warn().Msg("Gas price is 0. If Legacy estimations fail, there will no fallback price and transactions will start fail. Set gas price in config and disable EIP1559DynamicFees")
				}
				m.Cfg.Network.EIP1559DynamicFees = false
				calculateLegacyFees()
			}
		} else {
			estimations.GasFeeCap = maxFee
			estimations.GasTipCap = priorityFee
		}
	} else {
		calculateLegacyFees()
	}

	return estimations
}

// EstimateGasLimitForFundTransfer estimates gas limit for fund transfer
func (m *Client) EstimateGasLimitForFundTransfer(from, to common.Address, amount *big.Int) (uint64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), m.Cfg.Network.TxnTimeout.Duration())
	defer cancel()
	gasLimit, err := m.Client.EstimateGas(ctx, ethereum.CallMsg{
		From:  from,
		To:    &to,
		Value: amount,
	})
	if err != nil {
		L.Warn().Err(err).Msg("Failed to estimate gas for fund transfer.")
		return 0, errors.Wrapf(err, "failed to estimate gas for fund transfer")
	}
	return gasLimit, nil
}

// configureTransactionOpts configures transaction for legacy or type-2
func (m *Client) configureTransactionOpts(
	opts *bind.TransactOpts,
	nonce uint64,
	estimations GasEstimations,
	o ...TransactOpt,
) *bind.TransactOpts {
	opts.Nonce = big.NewInt(int64(nonce))
	opts.GasPrice = estimations.GasPrice
	opts.GasLimit = m.Cfg.Network.GasLimit

	if m.Cfg.Network.EIP1559DynamicFees {
		opts.GasPrice = nil
		opts.GasTipCap = estimations.GasTipCap
		opts.GasFeeCap = estimations.GasFeeCap
	}
	for _, f := range o {
		f(opts)
	}
	return opts
}

// ContractLoader is a helper struct for loading contracts
type ContractLoader[T any] struct {
	Client *Client
}

// NewContractLoader creates a new contract loader
func NewContractLoader[T any](client *Client) *ContractLoader[T] {
	return &ContractLoader[T]{
		Client: client,
	}
}

// LoadContract loads contract by name, address, ABI loader and wrapper init function, it adds contract ABI to Seth Contract Store and address to Contract Map. Thanks to that we can easily
// trace and debug interactions with the contract. Signatures of functions passed to this method were chosen to conform to Geth wrappers' GetAbi() and NewXXXContract() functions.
func (cl *ContractLoader[T]) LoadContract(name string, address common.Address, abiLoadFn func() (*abi.ABI, error), wrapperInitFn func(common.Address, bind.ContractBackend) (*T, error)) (*T, error) {
	abiData, err := abiLoadFn()
	if err != nil {
		return new(T), err
	}
	cl.Client.ContractStore.AddABI(name, *abiData)
	cl.Client.ContractAddressToNameMap.AddContract(address.Hex(), name)

	return wrapperInitFn(address, cl.Client.Client)
}

// DeployContract deploys contract using ABI and bytecode passed to it, waits for transaction to be minted and contract really
// available at the address, so that when the method returns it's safe to interact with it. It also saves the contract address and ABI name
// to the contract map, so that we can use that, when tracing transactions. It is suggested to use name identical to the name of the contract Solidity file.
func (m *Client) DeployContract(auth *bind.TransactOpts, name string, abi abi.ABI, bytecode []byte, params ...interface{}) (DeploymentData, error) {
	L.Info().
		Msgf("Started deploying %s contract", name)

	if auth.Context != nil {
		if err, ok := auth.Context.Value(ContextErrorKey{}).(error); ok {
			return DeploymentData{}, errors.Wrapf(err, "aborted contract deployment for %s, because context passed in transaction options had an error set", name)
		}
	}

	address, tx, contract, err := bind.DeployContract(auth, abi, bytecode, m.Client, params...)
	if err != nil {
		return DeploymentData{}, wrapErrInMessageWithASuggestion(err)
	}

	L.Info().
		Str("Address", address.Hex()).
		Str("TXHash", tx.Hash().Hex()).
		Msgf("Waiting for %s contract deployment to finish", name)

	m.ContractAddressToNameMap.AddContract(address.Hex(), name)

	if _, ok := m.ContractStore.GetABI(name); !ok {
		m.ContractStore.AddABI(name, abi)
	}

	// I had this one failing sometimes, when transaction has been minted, but contract cannot be found yet at address
	if err := retry.Do(
		func() error {
			ctx, cancel := context.WithTimeout(context.Background(), m.Cfg.Network.TxnTimeout.Duration())
			_, err := bind.WaitDeployed(ctx, m.Client, tx)
			cancel()

			// let's make sure that deployment transaction was successful, before retrying
			if err != nil {
				receipt, mineErr := bind.WaitMined(context.Background(), m.Client, tx)
				if mineErr != nil {
					return mineErr
				}

				if receipt.Status == 0 {
					return errors.New("deployment transaction was reverted")
				}
			}

			return err
		}, retry.OnRetry(func(i uint, _ error) {
			L.Debug().Uint("Attempt", i).Msg("Waiting for contract to be deployed")
		}),
		retry.DelayType(retry.FixedDelay),
		retry.Attempts(10),
		retry.Delay(time.Duration(1)*time.Second),
		retry.RetryIf(func(err error) bool {
			return strings.Contains(strings.ToLower(err.Error()), "no contract code at given address") ||
				strings.Contains(strings.ToLower(err.Error()), "no contract code after deployment")
		}),
	); err != nil {
		// do not pass the error here, because it's not transaction submission error
		_, _ = m.Decode(tx, nil)
		return DeploymentData{}, wrapErrInMessageWithASuggestion(err)
	}

	L.Info().
		Str("Address", address.Hex()).
		Str("TXHash", tx.Hash().Hex()).
		Msgf("Deployed %s contract", name)

	if !m.Cfg.ShoulSaveDeployedContractMap() {
		return DeploymentData{Address: address, Transaction: tx, BoundContract: contract}, nil
	}

	if err := SaveDeployedContract(m.Cfg.ContractMapFile, name, address.Hex()); err != nil {
		L.Warn().
			Err(err).
			Msg("Failed to save deployed contract address to file")
	}

	return DeploymentData{Address: address, Transaction: tx, BoundContract: contract}, nil
}

type DeploymentData struct {
	Address       common.Address
	Transaction   *types.Transaction
	BoundContract *bind.BoundContract
}

// DeployContractFromContractStore deploys contract from Seth's Contract Store, waits for transaction to be minted and contract really
// available at the address, so that when the method returns it's safe to interact with it. It also saves the contract address and ABI name
// to the contract map, so that we can use that, when tracing transactions. Name by which you refer the contract should be the same as the
// name of ABI file (you can omit the .abi suffix).
func (m *Client) DeployContractFromContractStore(auth *bind.TransactOpts, name string, params ...interface{}) (DeploymentData, error) {
	if m.ContractStore == nil {
		return DeploymentData{}, errors.New("ABIStore is nil")
	}

	name = strings.TrimSuffix(name, ".abi")
	name = strings.TrimSuffix(name, ".bin")

	abi, ok := m.ContractStore.ABIs[name+".abi"]
	if !ok {
		return DeploymentData{}, errors.New("ABI not found")
	}

	bytecode, ok := m.ContractStore.BINs[name+".bin"]
	if !ok {
		return DeploymentData{}, errors.New("BIN not found")
	}

	data, err := m.DeployContract(auth, name, abi, bytecode, params...)
	if err != nil {
		return DeploymentData{}, err
	}

	return data, nil
}

func (m *Client) SaveDecodedCallsAsJson(dirname string) error {
	return m.Tracer.SaveDecodedCallsAsJson(dirname)
}

type TransactionLog struct {
	Topics []common.Hash
	Data   []byte
}

func (t TransactionLog) GetTopics() []common.Hash {
	return t.Topics
}

func (t TransactionLog) GetData() []byte {
	return t.Data
}

func (m *Client) decodeContractLogs(l zerolog.Logger, logs []types.Log, a abi.ABI) ([]DecodedTransactionLog, error) {
	l.Trace().Msg("Decoding events")
	var eventsParsed []DecodedTransactionLog
	for _, lo := range logs {
		for _, evSpec := range a.Events {
			if evSpec.ID.Hex() == lo.Topics[0].Hex() {
				d := TransactionLog{lo.Topics, lo.Data}
				l.Trace().Str("Name", evSpec.RawName).Str("Signature", evSpec.Sig).Msg("Unpacking event")
				eventsMap, topicsMap, err := decodeEventFromLog(l, a, evSpec, d)
				if err != nil {
					return nil, errors.Wrap(err, ErrDecodeLog)
				}
				parsedEvent := decodedLogFromMaps(&DecodedTransactionLog{}, eventsMap, topicsMap)
				if decodedTransactionLog, ok := parsedEvent.(*DecodedTransactionLog); ok {
					decodedTransactionLog.Signature = evSpec.Sig
					m.mergeLogMeta(decodedTransactionLog, lo)
					eventsParsed = append(eventsParsed, *decodedTransactionLog)
					l.Trace().Interface("Log", parsedEvent).Msg("Transaction log")
				} else {
					l.Trace().
						Str("Actual type", fmt.Sprintf("%T", decodedTransactionLog)).
						Msg("Failed to cast decoded event to DecodedCommonLog")
				}
			}
		}
	}
	return eventsParsed, nil
}

// mergeLogMeta add metadata from log
func (m *Client) mergeLogMeta(pe *DecodedTransactionLog, l types.Log) {
	pe.Address = l.Address
	pe.Topics = make([]string, 0)
	for _, t := range l.Topics {
		pe.Topics = append(pe.Topics, t.String())
	}
	pe.BlockNumber = l.BlockNumber
	pe.Index = l.Index
	pe.TXHash = l.TxHash.Hex()
	pe.TXIndex = l.TxIndex
	pe.Removed = l.Removed
}
