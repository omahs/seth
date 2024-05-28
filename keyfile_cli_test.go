package seth_test

import (
	"context"
	"math/big"
	"os"
	"testing"

	"github.com/smartcontractkit/seth"
	sethcmd "github.com/smartcontractkit/seth/cmd"
	"github.com/stretchr/testify/require"
)

func AssertFileBalances(t *testing.T, amount *big.Int, keyfilePath string) {
	c := newClient(t)
	c.Cfg.KeyFilePath = keyfilePath
	kf, err := c.CreateOrUnmarshalKeyFile(nil)
	require.NoError(t, err)
	for _, kfd := range kf.Keys {
		require.NotEmpty(t, kfd.PrivateKey, "Private key is empty")
		require.NotEmpty(t, kfd.Address, "Address is empty")
		require.NotEmpty(t, kfd.Funds, "Funds is empty")
		if amount != nil {
			require.Equal(t, amount.String(), kfd.Funds, "Keyfile balance is incorrect")
		}
	}
}

func TestCLIFundAndReturn(t *testing.T) {
	keyFilePath := "keyfile_test.toml"
	_ = os.Remove(keyFilePath)
	err := os.Setenv(seth.KEYFILE_PATH_ENV_VAR, keyFilePath)
	require.NoError(t, err, "Error setting env var")
	c := newClient(t)
	for i := 0; i < 3; i++ {
		gasPrice, err := c.GetSuggestedLegacyFees(context.Background(), seth.Priority_Standard)
		if err != nil {
			gasPrice = big.NewInt(c.Cfg.Network.GasPrice)
		}
		bd, err := c.CalculateSubKeyFunding(10, gasPrice.Int64(), 10)
		require.NoError(t, err, "Error calculating subkey funding")
		err = sethcmd.RunCLI([]string{"seth", "-n", os.Getenv(seth.NETWORK_ENV_VAR), "keys", "fund", "-a", "10", "-b", "10"})
		require.NoError(t, err, "Error splitting keys")
		AssertFileBalances(t, bd.AddrFunding, keyFilePath)
		err = sethcmd.RunCLI([]string{"seth", "-n", os.Getenv(seth.NETWORK_ENV_VAR), "keys", "return"})
		require.NoError(t, err, "Error returning keys")
		AssertFileBalances(t, big.NewInt(0), keyFilePath)
	}
}

func TestCLIUpdateBalances(t *testing.T) {
	keyFilePath := "keyfile_test_2.toml"
	_ = os.Remove(keyFilePath)
	err := os.Setenv(seth.KEYFILE_PATH_ENV_VAR, keyFilePath)
	require.NoError(t, err)
	err = sethcmd.RunCLI([]string{"seth", "-n", os.Getenv(seth.NETWORK_ENV_VAR), "keys", "fund", "-a", "2", "-b", "10"})
	require.NoError(t, err)
	c := newClientWithKeyfile(t, keyFilePath)
	_, err = c.Decode(
		TestEnv.DebugContract.Pay(
			c.NewTXKeyOpts(2, seth.WithValue(big.NewInt(1e9))),
		),
	)
	require.NoError(t, err)
	err = sethcmd.RunCLI([]string{"seth", "-n", os.Getenv(seth.NETWORK_ENV_VAR), "keys", "update"})
	require.NoError(t, err)
	kf, err := c.CreateOrUnmarshalKeyFile(nil)
	require.NoError(t, err)
	require.NotEqual(t, kf.Keys[0].Funds, kf.Keys[1].Funds)
	err = sethcmd.RunCLI([]string{"seth", "-n", os.Getenv(seth.NETWORK_ENV_VAR), "keys", "return"})
	require.NoError(t, err)
}
