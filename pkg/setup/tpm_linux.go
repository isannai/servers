//go:build !windows

package setup

import (
	"fmt"
	"io"
	"os"
)

func openTPM() (io.ReadWriteCloser, error) {
	for _, dev := range []string{"/dev/tpmrm0", "/dev/tpm0"} {
		f, err := os.OpenFile(dev, os.O_RDWR, 0)
		if err == nil {
			return f, nil
		}
	}
	return nil, fmt.Errorf("no TPM device found (/dev/tpmrm0, /dev/tpm0)")
}

// readEK and decryptWithEK use the same go-tpm-tools API as Windows.
// The only difference is how the TPM device is opened (file vs TBS API).
// These are intentionally left unimplemented until Linux TPM testing
// environment is available — openTPM() above is ready, but client.EndorsementKeyRSA
// requires the rwc to be a proper TPM transport, not a raw file handle.
// TODO: integrate go-tpm-tools/client for Linux when test environment available.

func readEK(rwc io.ReadWriteCloser) (*ekHandle, error) {
	return nil, fmt.Errorf("TPM EK reading not yet tested on Linux")
}

func decryptWithEK(rwc io.ReadWriteCloser, credBlob, encSecret []byte) ([]byte, error) {
	return nil, fmt.Errorf("TPM ActivateCredential not yet tested on Linux")
}

// ReadAKPublicHash returns the AK's Name hash.
func ReadAKPublicHash() ([]byte, error) {
	return nil, fmt.Errorf("TPM AK not yet tested on Linux")
}
