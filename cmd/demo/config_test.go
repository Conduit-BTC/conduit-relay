package main

import (
	"testing"

	"conduitl2"
	"github.com/stretchr/testify/require"
)

func TestLoadRuntimeConfigParsesGiftWrapProtectionMode(t *testing.T) {
	t.Setenv("FLY_MACHINE_ID", "")
	t.Setenv("GIFT_WRAP_SINGLE_MACHINE_ID", "")

	for _, test := range []struct {
		name string
		raw  string
		want conduitl2.GiftWrapProtectionMode
	}{
		{name: "default", raw: "", want: conduitl2.GiftWrapProtectionDisabled},
		{name: "disabled", raw: "disabled", want: conduitl2.GiftWrapProtectionDisabled},
		{name: "challenge-only", raw: "challenge-only", want: conduitl2.GiftWrapProtectionChallengeOnly},
		{name: "enforce", raw: "enforce", want: conduitl2.GiftWrapProtectionEnforce},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("NIP42_GIFTWRAP_MODE", test.raw)
			cfg, err := loadRuntimeConfig()
			require.NoError(t, err)
			require.Equal(t, test.want, cfg.GiftWrapProtection)
		})
	}

	t.Setenv("NIP42_GIFTWRAP_MODE", "unknown")
	_, err := loadRuntimeConfig()
	require.Error(t, err)
}

func TestValidateGiftWrapSingleMachine(t *testing.T) {
	require.NoError(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionDisabled, "", ""))
	require.NoError(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionDisabled, "machine-a", "machine-a"))
	require.NoError(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionChallengeOnly, "", ""))
	require.NoError(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionChallengeOnly, "machine-a", "machine-a"))
	require.NoError(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionEnforce, "machine-a", "machine-a"))
	require.Error(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionDisabled, "machine-a", ""))
	require.Error(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionDisabled, "machine-a", "machine-b"))
	require.Error(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionChallengeOnly, "machine-a", ""))
	require.Error(t, validateGiftWrapSingleMachine(conduitl2.GiftWrapProtectionEnforce, "machine-a", "machine-b"))
}
