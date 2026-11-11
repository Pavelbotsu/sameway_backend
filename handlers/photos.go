package handlers

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png" // registers PNG for image.Decode
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Profile photos live on our own server (config.PhotoDir) instead of
// Firebase Storage, so no open Storage rules are needed (audit P0-12).
// Every upload is decoded and re-encoded: that strips EXIF metadata,
// including the GPS position phones write into photos (often the user's
// home), and rejects anything that isn't really an image.
const (
	maxPhotoUploadBytes = 3 << 20 // the app sends ≤512 px JPEGs, ~100 KB
	maxPhotoSourcePx    = 8000    // refuse decompression bombs before decoding
	photoMaxSidePx      = 512
	photoJPEGQuality    = 85
	photoURLPrefix      = "/photos/"
)

// photoFileRe matches the stored file names: <uuid>.jpg.
var photoFileRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jpg$`)

// UploadProfilePhoto: POST /auth/profile/photo, multipart field "photo"
// (JPEG or PNG). Responds {"photo_url": "/photos/<uuid>.jpg"}.
func UploadProfilePhoto(c *gin.Context) {
	userID := c.GetString("user_id")
	fh, err := c.FormFile("photo")
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo is too large (max 3 MB)"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "send the image in the 'photo' field"})
		return
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot read photo"})
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxPhotoUploadBytes+1))
	if err != nil || len(raw) > maxPhotoUploadBytes {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "photo is too large (max 3 MB)"})
		return
	}

	out, err := normalizePhoto(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "the file must be a JPEG or PNG image", "code": "bad_image"})
		return
	}

	name := uuid.NewString() + ".jpg"
	if err := os.MkdirAll(config.C.PhotoDir, 0o755); err != nil {
		log.Printf("UploadProfilePhoto: mkdir %s: %v", config.C.PhotoDir, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage error"})
		return
	}
	if err := os.WriteFile(filepath.Join(config.C.PhotoDir, name), out, 0o644); err != nil {
		log.Printf("UploadProfilePhoto: write: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "storage error"})
		return
	}

	url := photoURLPrefix + name
	var old *string
	err = db.DB.QueryRow(c.Request.Context(), `
		WITH prev AS (SELECT photo_url FROM users WHERE id = $1)
		UPDATE users SET photo_url = $2 WHERE id = $1 AND deleted_at IS NULL
		RETURNING (SELECT photo_url FROM prev)`, userID, url).Scan(&old)
	if err != nil {
		_ = os.Remove(filepath.Join(config.C.PhotoDir, name))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	if old != nil {
		removeStoredPhoto(*old)
	}
	c.JSON(http.StatusOK, gin.H{"photo_url": url})
}

// ServePhoto: GET /photos/:file. Public, like any avatar URL: the file name
// is a random UUID and the image carries no metadata.
func ServePhoto(c *gin.Context) {
	name := c.Param("file")
	if !photoFileRe.MatchString(name) {
		c.Status(http.StatusNotFound)
		return
	}
	path := filepath.Join(config.C.PhotoDir, name)
	if _, err := os.Stat(path); err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Header("Cache-Control", "public, max-age=86400")
	c.Header("X-Content-Type-Options", "nosniff")
	c.File(path)
}

// removeStoredPhoto deletes a photo previously stored by UploadProfilePhoto.
// URLs that aren't ours (old Firebase links) are ignored.
func removeStoredPhoto(url string) {
	name, ok := strings.CutPrefix(url, photoURLPrefix)
	if !ok || !photoFileRe.MatchString(name) {
		return
	}
	if err := os.Remove(filepath.Join(config.C.PhotoDir, name)); err != nil && !os.IsNotExist(err) {
		log.Printf("remove photo %s: %v", name, err)
	}
}

// removeUserPhoto deletes the user's stored photo file, if any. Used by
// account deletion before photo_url is cleared.
func removeUserPhoto(ctx context.Context, userID string) {
	var url *string
	if err := db.DB.QueryRow(ctx, `SELECT photo_url FROM users WHERE id = $1`, userID).Scan(&url); err == nil && url != nil {
		removeStoredPhoto(*url)
	}
}

// normalizePhoto decodes a JPEG/PNG, shrinks it to photoMaxSidePx and
// re-encodes it as JPEG. Only pixels survive: no EXIF, no GPS, no
// embedded payloads.
func normalizePhoto(raw []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxPhotoSourcePx || cfg.Height > maxPhotoSourcePx {
		return nil, errors.New("image dimensions out of range")
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	img := shrink(src, photoMaxSidePx)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: photoJPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// shrink scales src so its longer side is at most maxSide, averaging the
// source pixels that fall into each target pixel (box filter). Smaller
// images are copied as they are.
func shrink(src image.Image, maxSide int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	scale := 1.0
	if w > maxSide || h > maxSide {
		if w >= h {
			scale = float64(maxSide) / float64(w)
		} else {
			scale = float64(maxSide) / float64(h)
		}
	}
	tw, th := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	for ty := 0; ty < th; ty++ {
		y0 := b.Min.Y + ty*h/th
		y1 := max(y0+1, b.Min.Y+(ty+1)*h/th)
		for tx := 0; tx < tw; tx++ {
			x0 := b.Min.X + tx*w/tw
			x1 := max(x0+1, b.Min.X+(tx+1)*w/tw)
			var r, g, bl, a, n uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					cr, cg, cb, ca := src.At(x, y).RGBA()
					r, g, bl, a = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca)
					n++
				}
			}
			dst.SetRGBA(tx, ty, color.RGBA{
				R: uint8(r / n >> 8), G: uint8(g / n >> 8), B: uint8(bl / n >> 8), A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}
