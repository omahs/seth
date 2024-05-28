package seth_test

import (
	"github.com/ethereum/go-ethereum/common"
	link_token "github.com/smartcontractkit/seth/contracts/bind/link"
	"testing"

	"github.com/smartcontractkit/seth"
	"github.com/stretchr/testify/require"
)

func TestRPCHealtCheckEnabled_Node_OK(t *testing.T) {
	cfg, err := seth.ReadConfig()
	require.NoError(t, err, "failed to read config")
	cfg.CheckRpcHealthOnStart = true

	_, err = seth.NewClientWithConfig(cfg)
	require.NoError(t, err, "failed to initalise seth")
}

func TestRPCHealtCheckDisabled_Node_OK(t *testing.T) {
	cfg, err := seth.ReadConfig()
	require.NoError(t, err, "failed to read config")
	cfg.CheckRpcHealthOnStart = false

	_, err = seth.NewClientWithConfig(cfg)
	require.NoError(t, err, "failed to initalise seth")
}

func TestRPCHealtCheckEnabled_Node_Unhealthy(t *testing.T) {
	cfg, err := seth.ReadConfig()
	require.NoError(t, err, "failed to read config")

	newPks, err := seth.NewEphemeralKeys(1)
	require.NoError(t, err, "failed to create ephemeral keys")

	cfg.CheckRpcHealthOnStart = true
	cfg.Network.PrivateKeys = []string{newPks[0]}

	_, err = seth.NewClientWithConfig(cfg)
	require.Error(t, err, "expected error when connecting to unhealthy node")
	require.Contains(t, err.Error(), seth.ErrRpcHealthCheckFailed, "expected error message when connecting to dead node")
}

func TestRPCHealtCheckDisabled_Node_Unhealthy(t *testing.T) {
	cfg, err := seth.ReadConfig()
	require.NoError(t, err, "failed to read config")

	newPks, err := seth.NewEphemeralKeys(1)
	require.NoError(t, err, "failed to create ephemeral keys")

	cfg.CheckRpcHealthOnStart = false
	cfg.Network.PrivateKeys = []string{newPks[0]}

	_, err = seth.NewClientWithConfig(cfg)
	require.NoError(t, err, "expected health check to be skipped")
}

func TestContractLoader(t *testing.T) {
	c, err := seth.NewClient()
	require.NoError(t, err, "failed to initalise seth")

	loader := seth.NewContractLoader[link_token.LinkToken](c)

	contract, err := loader.LoadContract("LinkToken", TestEnv.LinkTokenContract.Address(), link_token.LinkTokenMetaData.GetAbi, link_token.NewLinkToken)
	require.NoError(t, err, "failed to load contract")

	owner, err := contract.Owner(c.NewCallOpts())
	require.NoError(t, err, "failed to call loaded LINK contract")
	require.NotEqual(t, common.Address{}, owner, "expected owner to be set")
}
