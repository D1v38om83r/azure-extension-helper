package decrypt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/Azure/VMApplication-Extension/VmExtensionHelper/extensionerrors"
	"golang.org/x/sys/windows"
	"strconv"
	"syscall"
	"unsafe"
)

const (
	certHashPropID        = 3
	crypteEAsn1BadTag     = 2148086027
)

var (
	modcrypt32                            = syscall.NewLazyDLL("crypt32.dll")
	procCertGetCertificateContextProperty = modcrypt32.NewProc("CertGetCertificateContextProperty")
	procCryptDecryptMessage               = modcrypt32.NewProc("CryptDecryptMessage")
)

type cryptDecryptMessagePara struct {
	cbSize                   uint32
	dwMsgAndCertEncodingType uint32
	cCertStore               uint32
	rghCertStore             uintptr
	dwFlags                  uint32
}

// decryptProtectedSettings decrypts the read protected settings using certificates
func DecryptProtectedSettings(configFolder string, thumbprint string, decoded []byte) (map[string]interface{}, error) {
	// Open My/Local
	handle, err := syscall.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM, 0, 0, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE, uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("MY"))))
	if err != nil {
		return nil, fmt.Errorf("VMextension: Cannot open certificate store due to '%v'", err)
	}
	if handle == 0 {
		return nil, extensionerrors.ErrMustRunAsAdmin
	}
	defer syscall.CertCloseStore(handle, 0)

	// Convert the thumbprint to bytes. We do byte comparison vs string comparison because otherwise we'd need to normalize the strings
	decodedThumbprint, err := thumbprintStringToHex(thumbprint)
	if err != nil {
		return nil, fmt.Errorf("VmExtension: Invalid thumbprint")
	}

	// Find the certificate by thumbprint
	const crypteENotFound = 0x80092004
	var cert *syscall.CertContext
	var prevContext *syscall.CertContext
	found := false
	for {
		// Keep retrieving the next certificate
		cert, err := syscall.CertEnumCertificatesInStore(handle, prevContext)
		if err != nil {
			if errno, ok := err.(syscall.Errno); ok {
				if errno == crypteENotFound {
					// We've reached the last certificate
					break
				}
			}
			return nil, fmt.Errorf("VmExtension: Could not enumerate certificates due to '%v'", err)
		}

		if cert == nil {
			break
		}

		// Determine the cert thumbprint
		foundthumbprint, err := getCertificateThumbprint(cert)
		if err == nil && foundthumbprint != nil {
			// TODO: consider logging if we have an error. For now, we just ignore the cert
			if bytes.Compare(decodedThumbprint, foundthumbprint) == 0 {
				found = true
				break
			}
		}

		prevContext = cert
	}

	if !found {
		return nil, extensionerrors.ErrCertWithThumbprintNotFound
	}

	// Decrypt the protected settings
	decryptedBytes, err := decryptDataWithCert(decoded, cert, uintptr(handle))
	if err != nil {
		return nil, err
	}

	// Now deserialize the data
	var v map[string]interface{}
	err = json.Unmarshal(decryptedBytes, &v)
	return v, err
}

func thumbprintStringToHex(s string) ([]byte, error) {
	// Remove the UTF mark if we have one
	runes := []rune(s)
	if len(runes)%2 == 1 {
		runes = []rune(s)[1:]
	}

	length := len(runes) / 2
	parts := make([]byte, length)
	for count := 0; count < length; count++ {
		r := runes[count*2 : count*2+2]
		sp := string(r)
		bp, err := strconv.ParseUint(sp, 16, 16)
		if err == nil {
			parts[count] = byte(bp)
		}
	}

	return parts, nil
}

// decryptDataWithCert calls the Windows APIs to do the decryption
func decryptDataWithCert(decoded []byte, cert *syscall.CertContext, storeHandle uintptr) ([]byte, error) {
	var cryptDecryptMessagePara cryptDecryptMessagePara
	cryptDecryptMessagePara.cbSize = uint32(len(decoded))
	cryptDecryptMessagePara.dwMsgAndCertEncodingType = uint32(windows.X509_ASN_ENCODING | windows.PKCS_7_ASN_ENCODING)
	cryptDecryptMessagePara.cCertStore = uint32(1)
	cryptDecryptMessagePara.rghCertStore = uintptr(unsafe.Pointer(&storeHandle))
	cryptDecryptMessagePara.dwFlags = uint32(0)

	// Call it once to get the decrypted data size
	var pbEncryptedBlob *byte
	var cbDecryptedBlob uint32
	pbEncryptedBlob = &decoded[0]
	raw, _, err := syscall.Syscall6(
		procCryptDecryptMessage.Addr(),
		6,
		uintptr(unsafe.Pointer(&cryptDecryptMessagePara)),
		uintptr(unsafe.Pointer(pbEncryptedBlob)),
		uintptr(len(decoded)),
		uintptr(0),
		uintptr(unsafe.Pointer(&cbDecryptedBlob)),
		uintptr(0),
	)
	if raw == 0 {
		errno := syscall.Errno(err)
		if errno == crypteEAsn1BadTag {
			return nil, extensionerrors.ErrInvalidProtectedSettingsData
		}

		return nil, fmt.Errorf("VmExtension: Could not decrypt data due to '%d'", syscall.Errno(err))
	}

	// Create our buffer
	if cbDecryptedBlob == 0 {
		return nil, nil
	}

	var decryptedBytes = make([]byte, cbDecryptedBlob)
	var pdecryptedBytes *byte
	pdecryptedBytes = &decryptedBytes[0]

	raw, _, err = syscall.Syscall6(
		procCryptDecryptMessage.Addr(),
		6,
		uintptr(unsafe.Pointer(&cryptDecryptMessagePara)),
		uintptr(unsafe.Pointer(pbEncryptedBlob)),
		uintptr(len(decoded)),
		uintptr(unsafe.Pointer(pdecryptedBytes)),
		uintptr(unsafe.Pointer(&cbDecryptedBlob)),
		uintptr(0),
	)
	if raw == 0 {
		return nil, fmt.Errorf("VmExtension: Could not decrypt data due to '%d'", syscall.Errno(err))
	}

	// Get rid of the null terminator or deserialization will fail
	returnedBytes := decryptedBytes[:cbDecryptedBlob]

	return returnedBytes, nil
}

// getCertificateThumbprint hashes the cert to obtain the thumbprint
func getCertificateThumbprint(cert *syscall.CertContext) ([]byte, error) {
	// Call it once to retrieve the thumbprint size
	var cbComputedHash uint32
	ret, _, err := syscall.Syscall6(
		procCertGetCertificateContextProperty.Addr(),
		4,
		uintptr(unsafe.Pointer(cert)),            // pCertContext
		uintptr(certHashPropID),                  // dwPropId
		uintptr(0),                               // pvData)
		uintptr(unsafe.Pointer(&cbComputedHash)), // pcbData
		0,
		0,
	)

	if ret == 0 {
		return nil, fmt.Errorf("VmExtension: Could not hash certificate due to '%d'", syscall.Errno(err))
	}

	// Create our buffer
	if cbComputedHash == 0 {
		return nil, nil
	}

	var computedHashBuffer = make([]byte, cbComputedHash)
	var pComputedHash *byte
	pComputedHash = &computedHashBuffer[0]
	ret, _, err = syscall.Syscall6(
		procCertGetCertificateContextProperty.Addr(),
		4,
		uintptr(unsafe.Pointer(cert)),            // pCertContext
		uintptr(certHashPropID),                  // dwPropId
		uintptr(unsafe.Pointer(pComputedHash)),   // pvData)
		uintptr(unsafe.Pointer(&cbComputedHash)), // pcbData
		0,
		0,
	)
	if ret == 0 {
		return nil, fmt.Errorf("VmExtension: Could not hash certificate due to '%d'", syscall.Errno(err))
	}

	return computedHashBuffer, nil
}