package handlers

import (
	"strings"
	"unicode/utf8"
)

// Profile field limits. Other users see these values, so they are bounded
// both to keep payloads small and to stop the profile being used as free-form
// storage. bio's 160 matches the app's maxLength.
const (
	maxNameLen     = 80
	maxCarFieldLen = 40
	maxBioLen      = 160
	maxPhoneLen    = 32
)

// validateProfileFields returns a user-facing error for the first invalid
// field, or "" when everything present is acceptable. nil means "not being
// changed" and is always fine.
func validateProfileFields(name, carMake, carModel, carColor, carPlate, photoURL, bio, phone *string) string {
	tooLong := func(p *string, n int) bool { return p != nil && utf8.RuneCountInString(*p) > n }
	switch {
	case tooLong(name, maxNameLen):
		return "name is too long"
	case tooLong(carMake, maxCarFieldLen), tooLong(carModel, maxCarFieldLen),
		tooLong(carColor, maxCarFieldLen), tooLong(carPlate, maxCarFieldLen):
		return "car details are too long"
	case tooLong(bio, maxBioLen):
		return "bio is too long (max 160 characters)"
	}
	if phone != nil && !validPhone(strings.TrimSpace(*phone)) {
		return "phone number is invalid"
	}
	if photoURL != nil && *photoURL != "" && !validPhotoURL(*photoURL) {
		return "upload photos with POST /auth/profile/photo"
	}
	return ""
}

// validPhone accepts "" (cleared) or a loosely formatted number: digits plus
// + - ( ) . and spaces, at most maxPhoneLen characters, at least 6 digits.
func validPhone(p string) bool {
	if p == "" {
		return true
	}
	if len(p) > maxPhoneLen {
		return false
	}
	digits := 0
	for _, r := range p {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case strings.ContainsRune("+-(). ", r):
		default:
			return false
		}
	}
	return digits >= 6
}

// validPhotoURL: through the profile endpoint a photo can only be removed
// (""). New photos go through UploadProfilePhoto, which stores and cleans
// them; accepting a URL here would let a profile point other users' apps
// at an arbitrary server (tracking pixel, huge file, etc.).
func validPhotoURL(raw string) bool {
	return raw == ""
}
