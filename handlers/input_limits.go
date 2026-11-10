package handlers

import (
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// maxRatingCommentLen bounds rating comments; the other party can read them.
const maxRatingCommentLen = 500

// maxPasswordBytes is bcrypt's input limit. Longer passwords make
// GenerateFromPassword fail, which used to surface as a 500.
const maxPasswordBytes = 72

var (
	dummyHashOnce sync.Once
	dummyHashVal  []byte
)

// dummyHash is a bcrypt hash at the cost real passwords use. Login compares
// against it when the email is unknown so both failure paths take the same
// time.
func dummyHash() []byte {
	dummyHashOnce.Do(func() {
		dummyHashVal, _ = bcrypt.GenerateFromPassword([]byte("sameway-timing-equaliser"), bcrypt.DefaultCost)
	})
	return dummyHashVal
}

// validateRegistration checks the free-text fields Register stores. Returns
// a user-facing error or "".
func validateRegistration(name, password string) string {
	n := strings.TrimSpace(name)
	switch {
	case n == "":
		return "name is required"
	case utf8.RuneCountInString(n) > maxNameLen:
		return "name is too long"
	case len(password) > maxPasswordBytes:
		return "password is too long (max 72 bytes)"
	}
	return ""
}
