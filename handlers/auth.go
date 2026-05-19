package handlers

import (
	"context"
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/db"
	"github.com/Pavelbotsu/sameway_backend/middleware"
	"github.com/Pavelbotsu/sameway_backend/models"
	"github.com/Pavelbotsu/sameway_backend/services"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

func Register(c *gin.Context) {
	var req struct {
		Name     string `json:"name" binding:"required"`
		Email    string `json:"email" binding:"required,email"`
		Password string `json:"password" binding:"required,min=6"`
		Role     string `json:"role" binding:"required,oneof=driver passenger"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}

	var id string
	err = db.DB.QueryRow(context.Background(),
		`INSERT INTO users (name, email, password, role) VALUES ($1,$2,$3,$4) RETURNING id`,
		req.Name, req.Email, string(hash), req.Role,
	).Scan(&id)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "email already registered"})
		return
	}

	token, err := middleware.GenerateToken(id, req.Role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusCreated, gin.H{"token": token, "user_id": id, "role": req.Role})
}

func Login(c *gin.Context) {
	var req struct {
		Email    string `json:"email" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var id, hash, role string
	err := db.DB.QueryRow(context.Background(),
		`SELECT id, password, role FROM users WHERE email = $1`,
		req.Email,
	).Scan(&id, &hash, &role)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	token, err := middleware.GenerateToken(id, role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "user_id": id, "role": role})
}

func GetProfile(c *gin.Context) {
	userID := c.GetString("user_id")
	var u models.User
	err := db.DB.QueryRow(context.Background(), `
		SELECT id, name, email, role,
		       COALESCE(car_make,''), COALESCE(car_model,''),
		       COALESCE(car_color,''), COALESCE(car_plate,'')
		FROM users WHERE id = $1`, userID,
	).Scan(&u.ID, &u.Name, &u.Email, &u.Role,
		&u.CarMake, &u.CarModel, &u.CarColor, &u.CarPlate)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, u)
}

func UpdateProfile(c *gin.Context) {
	var req struct {
		Name     string `json:"name"`
		CarMake  string `json:"car_make"`
		CarModel string `json:"car_model"`
		CarColor string `json:"car_color"`
		CarPlate string `json:"car_plate"`
	}
	c.ShouldBindJSON(&req)
	userID := c.GetString("user_id")
	_, err := db.DB.Exec(context.Background(), `
		UPDATE users SET
		    name      = CASE WHEN $1 <> '' THEN $1 ELSE name END,
		    car_make  = $2,
		    car_model = $3,
		    car_color = $4,
		    car_plate = $5
		WHERE id = $6`,
		req.Name, req.CarMake, req.CarModel, req.CarColor, req.CarPlate, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func SwitchRole(c *gin.Context) {
	var req struct {
		Role string `json:"role" binding:"required,oneof=driver passenger"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID := c.GetString("user_id")
	token, err := middleware.GenerateToken(userID, req.Role)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "role": req.Role, "user_id": userID})
}

func FCMStatus(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"fcm_enabled": services.IsFCMReady()})
}

func TestNotification(c *gin.Context) {
	userID := c.GetString("user_id")
	var fcmToken string
	db.DB.QueryRow(context.Background(),
		`SELECT COALESCE(fcm_token, '') FROM users WHERE id = $1`, userID,
	).Scan(&fcmToken)

	if err := services.SendTestNotification(fcmToken); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func GetStats(c *gin.Context) {
	userID := c.GetString("user_id")
	stats, err := services.GetUserStats(userID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "db error"})
		return
	}
	c.JSON(http.StatusOK, stats)
}

func UpdateFCMToken(c *gin.Context) {
	var req struct {
		FCMToken string `json:"fcm_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	userID := c.GetString("user_id")
	_, err := db.DB.Exec(context.Background(),
		`UPDATE users SET fcm_token = $1 WHERE id = $2`,
		req.FCMToken, userID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}
