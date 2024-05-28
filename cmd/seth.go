package seth

import (
	"context"
	"fmt"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/pelletier/go-toml/v2"
	"github.com/pkg/errors"
	"github.com/smartcontractkit/seth"
	"github.com/urfave/cli/v2"
	"math/big"
	"os"
	"path/filepath"
)

const (
	ErrNoNetwork = "no network specified, use -n flag. Ex.: 'seth -n Geth keys update' or -u and -c flags. Ex.: 'seth -u http://localhost:8545 -c 1337 keys update'"
)

var C *seth.Client

func RunCLI(args []string) error {
	app := &cli.App{
		Name:      "seth",
		Version:   "v1.0.0",
		Usage:     "seth CLI",
		UsageText: `utility to create and control Ethereum keys and give you more debug info about chains`,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "networkName", Aliases: []string{"n"}},
			&cli.StringFlag{Name: "url", Aliases: []string{"u"}},
		},
		Before: func(cCtx *cli.Context) error {
			networkName := cCtx.String("networkName")
			url := cCtx.String("url")
			if networkName == "" && url == "" {
				return errors.New(ErrNoNetwork)
			}
			if networkName != "" {
				_ = os.Setenv(seth.NETWORK_ENV_VAR, networkName)
			} else {
				_ = os.Setenv(seth.URL_ENV_VAR, url)
			}
			if cCtx.Args().Len() > 0 && cCtx.Args().First() != "trace" {
				var err error
				switch cCtx.Args().First() {
				case "keys":
					var cfg *seth.Config
					cfg, err = seth.ReadConfig()
					if err != nil {
						return err
					}
					keyfilePath := os.Getenv(seth.KEYFILE_PATH_ENV_VAR)
					if keyfilePath == "" {
						return fmt.Errorf("no keyfile path specified in %s env var", seth.KEYFILE_PATH_ENV_VAR)
					}
					cfg.KeyFileSource = seth.KeyFileSourceFile
					cfg.KeyFilePath = keyfilePath
					C, err = seth.NewClientWithConfig(cfg)
					if err != nil {
						return err
					}
				case "gas", "stats":
					var cfg *seth.Config
					var pk string
					_, pk, err = seth.NewAddress()
					if err != nil {
						return err
					}

					err = os.Setenv(seth.ROOT_PRIVATE_KEY_ENV_VAR, pk)
					if err != nil {
						return err
					}

					cfg, err = seth.ReadConfig()
					if err != nil {
						return err
					}
					C, err = seth.NewClientWithConfig(cfg)
					if err != nil {
						return err
					}
				case "trace":
					return nil
				}
				if err != nil {
					return err
				}
			}
			return nil
		},
		Commands: []*cli.Command{
			{
				Name:        "stats",
				HelpName:    "stats",
				Aliases:     []string{"s"},
				Description: "get various network related stats",
				Flags: []cli.Flag{
					&cli.Int64Flag{Name: "start_block", Aliases: []string{"s"}},
					&cli.Int64Flag{Name: "end_block", Aliases: []string{"e"}},
				},
				Action: func(cCtx *cli.Context) error {
					start := cCtx.Int64("start_block")
					end := cCtx.Int64("end_block")
					if start == 0 {
						return fmt.Errorf("at least start block should be defined, ex.: -s -10")
					}
					if start > 0 && end == 0 {
						return fmt.Errorf("invalid block params. Last N blocks example: -s -10, interval example: -s 10 -e 20")
					}
					cs, err := seth.NewBlockStats(C)
					if err != nil {
						return err
					}
					return cs.Stats(big.NewInt(start), big.NewInt(end))
				},
			},
			{
				Name:        "gas",
				HelpName:    "gas",
				Aliases:     []string{"g"},
				Description: "get various info about gas prices",
				Flags: []cli.Flag{
					&cli.Int64Flag{Name: "blocks", Aliases: []string{"b"}},
					&cli.Float64Flag{Name: "tipPercentile", Aliases: []string{"tp"}},
				},
				Action: func(cCtx *cli.Context) error {
					ge := seth.NewGasEstimator(C)
					blocks := cCtx.Uint64("blocks")
					tipPerc := cCtx.Float64("tipPercentile")
					stats, err := ge.Stats(blocks, tipPerc)
					if err != nil {
						return err
					}
					seth.L.Info().
						Interface("Max", stats.GasPrice.Max).
						Interface("99", stats.GasPrice.Perc99).
						Interface("75", stats.GasPrice.Perc75).
						Interface("50", stats.GasPrice.Perc50).
						Interface("25", stats.GasPrice.Perc25).
						Msg("Base fee (Wei)")
					seth.L.Info().
						Interface("Max", stats.TipCap.Max).
						Interface("99", stats.TipCap.Perc99).
						Interface("75", stats.TipCap.Perc75).
						Interface("50", stats.TipCap.Perc50).
						Interface("25", stats.TipCap.Perc25).
						Msg("Priority fee (Wei)")
					seth.L.Info().
						Interface("GasPrice", stats.SuggestedGasPrice).
						Msg("Suggested gas price now")
					seth.L.Info().
						Interface("GasTipCap", stats.SuggestedGasTipCap).
						Msg("Suggested gas tip cap now")

					type asTomlCfg struct {
						GasPrice int64 `toml:"gas_price"`
						GasTip   int64 `toml:"gas_tip_cap"`
						GasFee   int64 `toml:"gas_fee_cap"`
					}

					tomlCfg := asTomlCfg{
						GasPrice: stats.SuggestedGasPrice.Int64(),
						GasTip:   stats.SuggestedGasTipCap.Int64(),
						GasFee:   stats.SuggestedGasPrice.Int64() + stats.SuggestedGasTipCap.Int64(),
					}

					marshalled, err := toml.Marshal(tomlCfg)
					if err != nil {
						return err
					}

					seth.L.Info().Msgf("Fallback prices for TOML config:\n%s", string(marshalled))

					return err
				},
			},
			{
				Name:        "keys",
				HelpName:    "keys",
				Aliases:     []string{"k"},
				Description: "key management commands",
				ArgsUsage:   "",
				Subcommands: []*cli.Command{
					{
						Name:        "update",
						HelpName:    "update",
						Aliases:     []string{"u"},
						Description: "update balances for all the keys in keyfile.toml",
						ArgsUsage:   "seth keys update",
						Action: func(cCtx *cli.Context) error {
							return seth.UpdateKeyFileBalances(C)
						},
					},
					{
						Name:        "fund",
						HelpName:    "fund",
						Aliases:     []string{"f"},
						Description: "create a new key file, split the funds from the root account to new keys OR fund existing keys read from keyfile",
						ArgsUsage:   "-a ${amount of addresses to create} -b ${amount in ethers to keep in root key}",
						Flags: []cli.Flag{
							&cli.Int64Flag{Name: "addresses", Aliases: []string{"a"}},
							&cli.Int64Flag{Name: "buffer", Aliases: []string{"b"}},
						},
						Action: func(cCtx *cli.Context) error {
							addresses := cCtx.Int64("addresses")
							rootKeyBuffer := cCtx.Int64("buffer")
							opts := &seth.FundKeyFileCmdOpts{Addrs: addresses, RootKeyBuffer: rootKeyBuffer}
							return seth.UpdateAndSplitFunds(C, opts)
						},
					},
					{
						Name:        "return",
						HelpName:    "return",
						Aliases:     []string{"r"},
						Description: "returns all the funds from addresses from keyfile.toml to original root key (KEYS env var)",
						ArgsUsage:   "-a ${addr_to_return_to}",
						Flags: []cli.Flag{
							&cli.StringFlag{Name: "address", Aliases: []string{"a"}},
						},
						Action: func(cCtx *cli.Context) error {
							toAddr := cCtx.String("address")
							return seth.ReturnFundsAndUpdateKeyfile(C, toAddr)
						},
					},
					{
						Name:        "remove",
						Aliases:     []string{"rm"},
						Description: "removes keyfile.toml",
						HelpName:    "return",
						Action: func(cCtx *cli.Context) error {
							return os.Remove(C.Cfg.KeyFilePath)
						},
					},
				},
			},
			{
				Name:        "trace",
				HelpName:    "trace",
				Aliases:     []string{"t"},
				Description: "trace transactions loaded from JSON file",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "file", Aliases: []string{"f"}},
				},
				Action: func(cCtx *cli.Context) error {
					file := cCtx.String("file")
					var transactions []string
					err := seth.OpenJsonFileAsStruct(file, &transactions)
					if err != nil {
						return err
					}

					_ = os.Setenv(seth.LogLevelEnvVar, "debug")

					cfgPath := os.Getenv(seth.CONFIG_FILE_ENV_VAR)
					if cfgPath == "" {
						return errors.New(seth.ErrEmptyConfigPath)
					}
					var cfg *seth.Config
					d, err := os.ReadFile(cfgPath)
					if err != nil {
						return errors.Wrap(err, seth.ErrReadSethConfig)
					}
					err = toml.Unmarshal(d, &cfg)
					if err != nil {
						return errors.Wrap(err, seth.ErrUnmarshalSethConfig)
					}
					absPath, err := filepath.Abs(cfgPath)
					if err != nil {
						return err
					}
					cfg.ConfigDir = filepath.Dir(absPath)

					snet := os.Getenv(seth.NETWORK_ENV_VAR)
					if snet != "" {
						for _, n := range cfg.Networks {
							if n.Name == snet {
								cfg.Network = n
								break
							}
						}
						if cfg.Network == nil {
							return fmt.Errorf("network %s not defined in the TOML file", snet)
						}
					} else {
						url := os.Getenv(seth.URL_ENV_VAR)

						if url == "" {
							return fmt.Errorf("network not selected, set %s=... or %s=..., check TOML config for available networks", seth.NETWORK_ENV_VAR, seth.URL_ENV_VAR)
						}

						//look for default network
						for _, n := range cfg.Networks {
							if n.Name == seth.DefaultNetworkName {
								cfg.Network = n
								cfg.Network.Name = snet
								cfg.Network.URLs = []string{url}
								break
							}
						}

						if cfg.Network == nil {
							return fmt.Errorf("default network not defined in the TOML file")
						}

						client, err := ethclient.Dial(cfg.Network.URLs[0])
						if err != nil {
							return fmt.Errorf("failed to connect to '%s' due to: %w", cfg.Network.URLs[0], err)
						}
						defer client.Close()

						if cfg.Network.ChainID == seth.DefaultNetworkName {
							chainId, err := client.ChainID(context.Background())
							if err != nil {
								return errors.Wrap(err, "failed to get chain ID")
							}
							cfg.Network.ChainID = chainId.String()
						}
					}

					zero := int64(0)
					cfg.EphemeralAddrs = &zero

					client, err := seth.NewClientWithConfig(cfg)
					if err != nil {
						return err
					}

					seth.L.Info().Msgf("Tracing transactions from %s file", file)

					for _, tx := range transactions {
						seth.L.Info().Msgf("Tracing transaction %s", tx)
						err = client.Tracer.TraceGethTX(tx)
						if err != nil {
							return err
						}
					}
					return err
				},
			},
		},
	}
	return app.Run(args)
}
