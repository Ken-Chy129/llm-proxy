package dashboard

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed static
var staticFiles embed.FS

func Handler() gin.HandlerFunc {
	// Serve bytes directly: http.FS redirects any "index.html" request to "./",
	// which loops forever on the root path.
	page, err := staticFiles.ReadFile("static/index.html")
	if err != nil {
		panic("dashboard: embedded index.html missing: " + err.Error())
	}
	return func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", page)
	}
}

func StaticFS() http.FileSystem {
	sub, _ := fs.Sub(staticFiles, "static")
	return http.FS(sub)
}
