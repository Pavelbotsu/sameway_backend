package handlers_test

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/testutil"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withPhotoDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := config.C.PhotoDir
	config.C.PhotoDir = dir
	t.Cleanup(func() { config.C.PhotoDir = prev })
	return dir
}

func testImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 120, 255})
		}
	}
	return img
}

// jpegWithGPS returns a JPEG carrying an EXIF (APP1) segment with a fake GPS
// marker, the way phone cameras embed the shooting location.
func jpegWithGPS(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, testImage(w, h), nil))
	raw := buf.Bytes()
	payload := []byte("Exif\x00\x00GPS-SECRET-52.2297,21.0122")
	seg := append([]byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
	out := append([]byte{}, raw[:2]...) // SOI
	out = append(out, seg...)
	return append(out, raw[2:]...)
}

func uploadPhoto(t *testing.T, r *gin.Engine, token, filename string, data []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("photo", filename)
	require.NoError(t, err)
	_, _ = fw.Write(data)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/auth/profile/photo", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestPhotoUpload_StripsMetadataAndShrinks(t *testing.T) {
	pool, r := setupHandlers(t)
	dir := withPhotoDir(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "photo-1")

	w := uploadPhoto(t, r, tok, "me.jpg", jpegWithGPS(t, 1200, 800))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]string
	testutil.DecodeJSON(t, w, &resp)
	url := resp["photo_url"]
	require.True(t, strings.HasPrefix(url, "/photos/"), url)

	stored, err := os.ReadFile(filepath.Join(dir, strings.TrimPrefix(url, "/photos/")))
	require.NoError(t, err)
	assert.NotContains(t, string(stored), "GPS-SECRET", "location metadata must be gone")
	assert.NotContains(t, string(stored), "Exif")
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(stored))
	require.NoError(t, err)
	assert.Equal(t, 512, cfg.Width)
	assert.Equal(t, 341, cfg.Height)

	// Profile points at it and it is served.
	var prof map[string]any
	testutil.DecodeJSON(t, testutil.GetJSON(t, r, "/auth/profile", tok), &prof)
	assert.Equal(t, url, prof["photo_url"])
	g := testutil.GetJSON(t, r, url, "")
	assert.Equal(t, http.StatusOK, g.Code)
	assert.Equal(t, "image/jpeg", g.Header().Get("Content-Type"))

	// A new upload replaces the file.
	w = uploadPhoto(t, r, tok, "me2.jpg", jpegWithGPS(t, 300, 300))
	require.Equal(t, http.StatusOK, w.Code)
	files, _ := os.ReadDir(dir)
	assert.Len(t, files, 1, "the previous photo is deleted")

	// Removing the photo deletes the file.
	require.Equal(t, http.StatusOK,
		testutil.PutJSON(t, r, "/auth/profile", tok, map[string]any{"photo_url": ""}).Code)
	files, _ = os.ReadDir(dir)
	assert.Empty(t, files)
}

func TestPhotoUpload_AcceptsPNGRejectsOthers(t *testing.T) {
	pool, r := setupHandlers(t)
	withPhotoDir(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "photo-2")

	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, testImage(64, 64)))
	assert.Equal(t, http.StatusOK, uploadPhoto(t, r, tok, "a.png", buf.Bytes()).Code)

	w := uploadPhoto(t, r, tok, "a.jpg", []byte("<html>not an image</html>"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "bad_image")

	w = uploadPhoto(t, r, tok, "big.jpg", bytes.Repeat([]byte{0xFF}, 4<<20))
	assert.Contains(t, []int{http.StatusRequestEntityTooLarge, http.StatusBadRequest}, w.Code)
}

func TestServePhoto_OnlyStoredNames(t *testing.T) {
	_, r := setupHandlers(t)
	withPhotoDir(t)
	for _, p := range []string{"/photos/..%2Fsecret", "/photos/x.jpg", "/photos/00000000-0000-0000-0000-000000000000.jpg"} {
		assert.Equal(t, http.StatusNotFound, testutil.GetJSON(t, r, p, "").Code, p)
	}
}

func TestDeleteAccount_RemovesPhotoFile(t *testing.T) {
	pool, r := setupHandlers(t)
	dir := withPhotoDir(t)
	_, tok := testutil.CreateUser(t, pool, "passenger", "photo-del")
	require.Equal(t, http.StatusOK, uploadPhoto(t, r, tok, "me.jpg", jpegWithGPS(t, 100, 100)).Code)

	w := testutil.PostJSON(t, r, "/auth/delete-account", tok, map[string]any{"password": "test-pass"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	files, _ := os.ReadDir(dir)
	assert.Empty(t, files)
}
