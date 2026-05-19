package handlers

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type Promotion struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Subtitle    string    `json:"subtitle"`
	DiscountPct int       `json:"discount_pct"`
	Partner     string    `json:"partner"`
	Category    string    `json:"category"` // "driver" | "passenger" | "both"
	IconName    string    `json:"icon_name"`
	ExpiresAt   time.Time `json:"expires_at"`
}

var staticPromotions = []Promotion{
	{
		ID:          "promo-1",
		Title:       "10% off Car Wash",
		Subtitle:    "Show this coupon at any CleanWave partner station after completing 3 rides.",
		DiscountPct: 10,
		Partner:     "CleanWave",
		Category:    "driver",
		IconName:    "local_car_wash",
		ExpiresAt:   time.Now().AddDate(0, 2, 0),
	},
	{
		ID:          "promo-2",
		Title:       "5% Fuel Discount",
		Subtitle:    "Save on every fill-up at OilPlus stations. Valid for active drivers.",
		DiscountPct: 5,
		Partner:     "OilPlus",
		Category:    "driver",
		IconName:    "local_gas_station",
		ExpiresAt:   time.Now().AddDate(0, 1, 0),
	},
	{
		ID:          "promo-3",
		Title:       "Free Coffee",
		Subtitle:    "Get a free coffee at BrewPoint after your 5th shared ride.",
		DiscountPct: 100,
		Partner:     "BrewPoint",
		Category:    "both",
		IconName:    "local_cafe",
		ExpiresAt:   time.Now().AddDate(0, 3, 0),
	},
	{
		ID:          "promo-4",
		Title:       "Free Parking — 1 Hour",
		Subtitle:    "ParkEasy gives you 1 free hour of parking per week for active drivers.",
		DiscountPct: 100,
		Partner:     "ParkEasy",
		Category:    "driver",
		IconName:    "local_parking",
		ExpiresAt:   time.Now().AddDate(0, 1, 15),
	},
	{
		ID:          "promo-5",
		Title:       "15% off Next Ride",
		Subtitle:    "Refer a friend to Sameway and get 15% off your next ride request.",
		DiscountPct: 15,
		Partner:     "Sameway",
		Category:    "passenger",
		IconName:    "card_giftcard",
		ExpiresAt:   time.Now().AddDate(0, 2, 0),
	},
}

func GetPromotions(c *gin.Context) {
	c.JSON(http.StatusOK, staticPromotions)
}
