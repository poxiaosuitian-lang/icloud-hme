package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Service API uses explicit bearer authentication, never browser cookies.
// Alias allocation/usage import and mail read operations are exposed; admin and delete
// endpoints remain session/CSRF protected. An empty token disables this API.
func (s *Server) registerServiceAPI(api *gin.RouterGroup) {
	v1 := api.Group("/v1", s.requireAPIToken())
	v1.GET("/accounts", s.listAccountsHandler)
	v1.GET("/aliases", s.listAliasesHandler)
	v1.POST("/aliases", s.allocateAliasHandler)
	v1.POST("/aliases/allocate", s.allocateAliasHandler)
	v1.GET("/aliases/usage", s.aliasUsageHandler)
	v1.POST("/aliases/usage", s.importAliasUsageHandler)
	v1.GET("/inbox", s.listInboxHandler)
	v1.GET("/inbox/:message_id", s.getMessageHandler)
}

func (s *Server) requireAPIToken() gin.HandlerFunc {
	return func(c *gin.Context) {
		settings := s.apiSettings.snapshot()
		token := settings.Token
		expected := sha256.Sum256([]byte(token))
		if !settings.Enabled || token == "" {
			failCode(c, http.StatusServiceUnavailable, "API_DISABLED", "API 服务未配置 Token")
			return
		}
		headers := c.Request.Header.Values("Authorization")
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(headers) != 1 || len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.Header("WWW-Authenticate", "Bearer")
			failCode(c, http.StatusUnauthorized, "API_TOKEN_INVALID", "需要有效的 Bearer Token")
			return
		}
		actual := sha256.Sum256([]byte(parts[1]))
		if subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
			c.Header("WWW-Authenticate", "Bearer")
			failCode(c, http.StatusUnauthorized, "API_TOKEN_INVALID", "需要有效的 Bearer Token")
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		c.Next()
	}
}
