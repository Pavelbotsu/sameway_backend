package handlers

import (
	"net/http"

	"github.com/Pavelbotsu/sameway_backend/config"
	"github.com/gin-gonic/gin"
)

// inServiceArea checks that every point lies in one and the same service
// area (config.ServiceAreas). Otherwise it writes 403 outside_service_area,
// listing the areas, and returns false. With no areas configured it always
// passes.
func inServiceArea(c *gin.Context, points ...[2]float64) bool {
	area := ""
	for i, p := range points {
		a := config.C.AreaFor(p[0], p[1])
		if a == "*" {
			return true
		}
		if a == "" || (i > 0 && a != area) {
			names := make([]string, 0, len(config.C.ServiceAreas))
			for _, sa := range config.C.ServiceAreas {
				names = append(names, sa.Name)
			}
			c.JSON(http.StatusForbidden, gin.H{
				"error": "Sameway isn't available here yet",
				"code":  "outside_service_area",
				"areas": names,
			})
			return false
		}
		area = a
	}
	return true
}
