package auth

import (
	"crypto/sha1"
)

// NativePassword computes mysql_native_password token.
// Port of go-mysql mysql.CalcNativePassword (scramble must be 20 bytes for native).
func NativePassword(password string, scramble []byte) []byte {
	if len(password) == 0 {
		return nil
	}
	// stage1 = SHA1(password)
	crypt := sha1.New()
	crypt.Write([]byte(password))
	stage1 := crypt.Sum(nil)

	// stage2 = SHA1(stage1)
	crypt.Reset()
	crypt.Write(stage1)
	stage2 := crypt.Sum(nil)

	// scrambleHash = SHA1(scramble + stage2)
	crypt.Reset()
	crypt.Write(scramble)
	crypt.Write(stage2)
	scrambleHash := crypt.Sum(nil)

	return xor(scrambleHash, stage1)
}

func xor(a, b []byte) []byte {
	out := make([]byte, len(a))
	for i := range a {
		out[i] = a[i] ^ b[i%len(b)]
	}
	return out
}
