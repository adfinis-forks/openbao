// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package namespaces

import (
	"fmt"
	"testing"
	"time"

	"github.com/openbao/openbao/api/v2"
	logicalKv "github.com/openbao/openbao/builtin/logical/kv"
	logicalPki "github.com/openbao/openbao/builtin/logical/pki"
	"github.com/openbao/openbao/helper/testhelpers/teststorage"

	vaulthttp "github.com/openbao/openbao/http"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/openbao/openbao/vault"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadOnlyStandby(t *testing.T) {
	conf := vault.CoreConfig{
		LogicalBackends: map[string]logical.Factory{
			"pki": logicalPki.Factory,
			"kv":  logicalKv.VersionedKVFactory,
		},
	}
	opts := vault.TestClusterOptions{
		HandlerFunc: vaulthttp.Handler,
	}

	teststorage.RaftBackendSetup(&conf, &opts)
	cluster := vault.NewTestCluster(t, &conf, &opts)

	cluster.Start()
	defer cluster.Cleanup()

	cores := cluster.Cores

	primaryClient := cluster.Cores[0].Client
	standbyClient := cluster.Cores[1].Client
	vault.TestWaitActive(t, cores[0].Core)
	t.Log("core0 now leader")

	require.Eventually(t, func() bool {
		t.Log("check core1 unsealed")
		return !cluster.Cores[1].Sealed()
	}, 10*time.Second, 100*time.Microsecond)
	t.Log("core1 now unsealed")

	require.NoError(t, primaryClient.Sys().Mount("kv", &api.MountInput{
		Type: "kv-v2",
	}))

	t.Log("sleeping")
	time.Sleep(10 * time.Second)
	t.Log("done sleeping")

	for i, core := range cluster.Cores {
		mounts, err := core.ListMounts()
		require.NoError(t, err)
		for j, mount := range mounts {
			t.Logf("c[%d].Mount[%d] = %s @ %s", i, j, mount.Type, mount.Path)
		}
	}

	fmt.Println("addresses", primaryClient.Address(), standbyClient.Address())
	require.NotEqual(t, primaryClient.Address(), standbyClient.Address())

	token, err := primaryClient.Auth().Token().CreateWithContext(t.Context(), &api.TokenCreateRequest{})
	require.NoError(t, err)
	standbyClient.SetToken(token.Auth.ClientToken)

	for i := range 2 {
		expectedValue := fmt.Sprintf("expected value %d", i)

		_, err := primaryClient.KVv2("secret").Put(t.Context(), "foo", map[string]any{ // TODO(phil9909): switch to "kv"
			"bar": expectedValue,
		})
		require.NoError(t, err)

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			t.Logf("primary trying %d", i)
			data, err := primaryClient.KVv2("secret").Get(t.Context(), "foo")
			require.NoError(collect, err)
			require.Equal(collect, expectedValue, data.Data["bar"])
		}, 10*time.Second, 1*time.Second, "should be empty")

		require.EventuallyWithT(t, func(collect *assert.CollectT) {
			t.Logf("standby trying %d", i)
			data, err := standbyClient.KVv2("secret").Get(t.Context(), "foo")
			require.NoError(collect, err)
			require.Equal(collect, expectedValue, data.Data["bar"])
		}, 10*time.Second, 1*time.Second, "should be empty")
	}

	t.Log("revoking token")
	require.NoError(t, primaryClient.Auth().Token().RevokeTreeWithContext(t.Context(), token.Auth.ClientToken))
	_, err = standbyClient.KVv2("secret").Get(t.Context(), "foo")
	require.ErrorContains(t, err, "permission denied", "token was revoked on the primary, should be declined by secondaries")
}
