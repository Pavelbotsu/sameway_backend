package handlers

import (
	"strings"
	"sync"
	"time"
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

// minAge is the minimum age to use Sameway (Terms: 18+, like BlaBlaCar).
const minAge = 18

// ageOn returns the age in whole years on day `on` of someone born on `birth`
// (calendar arithmetic; a 29 Feb birthday turns a year older on 1 Mar).
func ageOn(birth, on time.Time) int {
	years := on.Year() - birth.Year()
	if on.Month() < birth.Month() || (on.Month() == birth.Month() && on.Day() < birth.Day()) {
		years--
	}
	return years
}

// parseBirthDate validates a YYYY-MM-DD date of birth. On failure it returns
// a machine code ("birth_date_invalid" or "underage") and a message.
func parseBirthDate(s string, today time.Time) (time.Time, string, string) {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, "birth_date_invalid", "birth_date must be a date (YYYY-MM-DD)"
	}
	if d.After(today) || ageOn(d, today) > 100 {
		return time.Time{}, "birth_date_invalid", "birth_date is not plausible"
	}
	if ageOn(d, today) < minAge {
		return time.Time{}, "underage", "you must be 18 or older to use Sameway"
	}
	return d, "", ""
}
