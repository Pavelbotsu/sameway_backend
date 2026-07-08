package middleware

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

func Auth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := tokenFromRequest(c)
		if token == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing token"})
			return
		}
		userID, role, err := ParseToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		c.Set("user_id", userID)
		c.Set("role", role)
		c.Next()
	}
}

func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.GetString("role") != role {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Next()
	}
}

func GenerateToken(userID, role string) (string, error) {
	ttl := time.Duration(config.C.JWTTTLHours) * time.Hour
	if ttl <= 0 {
		ttl = 720 * time.Hour
	}
	now := time.Now()
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": userID,
		"role":    role,
		"iat":     now.Unix(),
		"exp":     now.Add(ttl).Unix(),
	})
	return t.SignedString([]byte(config.C.JWTSecret))
}

// ParseToken validates the token (signature, and expiry when present — tokens
// issued before the exp claim was introduced remain valid until users
// re-login) and returns the user_id and role claims. Both must be present
// and strings; a structurally valid token without them is rejected rather
// than panicking downstream.
func ParseToken(tokenStr string) (userID, role string, err error) {
	t, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		return []byte(config.C.JWTSecret), nil
	}, jwt.WithValidMethods([]string{"HS256", "HS384", "HS512"}))
	if err != nil {
		return "", "", err
	}
	if !t.Valid {
		return "", "", errors.New("invalid token")
	}
	claims, ok := t.Claims.(jwt.MapClaims)
	if !ok {
		return "", "", errors.New("unexpected claims type")
	}
	userID, uok := claims["user_id"].(string)
	role, rok := claims["role"].(string)
	if !uok || !rok || userID == "" || role == "" {
		return "", "", errors.New("token missing required claims")
	}
	return userID, role, nil
}

func tokenFromRequest(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return c.Query("token")
}
