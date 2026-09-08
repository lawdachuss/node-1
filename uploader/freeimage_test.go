package uploader

import (
	"errors"
	"os"
	"testing"
)

func TestFreeImageHasTokenRequiresEnvKey(t *testing.T) {
	setenv(t, "FREEIMAGEHOST_API_KEY", "")
	if NewFreeImageHostUploader().HasToken() {
		t.Fatal("HasToken() should be false when FREEIMAGEHOST_API_KEY is not set")
	}
	setenv(t, "FREEIMAGEHOST_API_KEY", "real-account-key")
	if !NewFreeImageHostUploader().HasToken() {
		t.Fatal("HasToken() should be true when FREEIMAGEHOST_API_KEY is set")
	}
}

func TestFreeImage403IsFailFast(t *testing.T) {
	// freeimage.host rejects programmatic uploads with the shared guest key
	// via HTTP 403 "requires authentication". Retrying the same host is futile,
	// so the upload chain must treat it as fail-fast and try the next host.
	cases := []string{
		"freeimage.host: HTTP 403: requires authentication",
		"freeimage.host: HTTP 403: forbidden",
		"freeimage.host: HTTP 403: access denied",
	}
	for _, c := range cases {
		if !isFailFastError(errors.New(c)) {
			t.Errorf("expected %q to be fail-fast", c)
		}
	}
}

// setenv sets an env var and runs the cleanup via t.Cleanup.
func setenv(t *testing.T, key, val string) {
	t.Helper()
	prev, ok := os.LookupEnv(key)
	os.Setenv(key, val)
	t.Cleanup(func() {
		if ok {
			os.Setenv(key, prev)
		} else {
			os.Unsetenv(key)
		}
	})
}
