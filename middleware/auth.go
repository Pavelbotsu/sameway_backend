package middleware

import (
	"net/http"
	"strings"

	"github.com/Pavelbotsu/rideshare-backend/config"
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
		claims, err := ParseToken(token)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
			return
		}
		c.Set("user_id", claims["user_id"].(string))
		c.Set("role", claims["role"].(string))
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
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id": userID,
		"role":    role,
	})
	return t.SignedString([]byte(config.C.JWTSecret))
}

func ParseToken(tokenStr string) (jwt.MapClaims, error) {
	t, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return []byte(config.C.JWTSecret), nil
	})
	if err != nil || !t.Valid {
		return nil, err
	}
	return t.Claims.(jwt.MapClaims), nil
}

func tokenFromRequest(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return c.Query("token")
}
