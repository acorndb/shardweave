package auth

import "crypto/sha256"

// CachingSHA2Password computes caching_sha2_password fast-auth token.
// Port of go-mysql mysql.CalcCachingSha2Password.
func CachingSHA2Password(password string, scramble []byte) []byte {
	if len(password) == 0 {
		return nil
	}
	// XOR(SHA256(password), SHA256(SHA256(SHA256(password)), scramble))
	crypt := sha256.New()
	crypt.Write([]byte(password))
	message1 := crypt.Sum(nil)

	crypt.Reset()
	crypt.Write(message1)
	message1Hash := crypt.Sum(nil)

	crypt.Reset()
	crypt.Write(message1Hash)
	crypt.Write(scramble)
	message2 := crypt.Sum(nil)

	return xor(message1, message2)
}
