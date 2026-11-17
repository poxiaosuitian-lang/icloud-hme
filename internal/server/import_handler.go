package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
)

const importPath = "/api/integrations/appleservices/accounts"
const importMaxBytes = 256 << 10

func ValidateImportToken(token string) error {
	if token != "" && (len(token) < 6 || len(token) > 512 || strings.IndexFunc(token, unicode.IsSpace) >= 0 || strings.IndexFunc(token, unicode.IsControl) >= 0) {
		return errors.New("ICLOUD_HME_IMPORT_TOKEN must contain 6–512 non-whitespace characters")
	}
	return nil
}

type appleServicesImporter interface {
	ImportAppleServices(context.Context, account.ImportRequest) (account.ImportResult, bool, error)
}

func (b *managerBackend) ImportAppleServices(ctx context.Context, r account.ImportRequest) (account.ImportResult, bool, error) {
	return b.mgr.ImportAppleServices(ctx, r)
}
func importFailure(c *gin.Context, status int, code string) {
	c.AbortWithStatusJSON(status, gin.H{"success": false, "code": code, "message": code})
}

func (s *Server) authorizeAppleServices(c *gin.Context) bool {
	if s.cfg.ImportToken == "" {
		importFailure(c, 404, "IMPORT_DISABLED")
		return false
	}
	key := c.ClientIP()
	if allowed, retry := s.importLimiter.Allow(key); !allowed {
		c.Header("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		importFailure(c, 429, "IMPORT_RATE_LIMITED")
		return false
	}
	auth := c.Request.Header.Values("Authorization")
	value := ""
	if len(auth) == 1 && strings.HasPrefix(auth[0], "Bearer ") {
		value = strings.TrimPrefix(auth[0], "Bearer ")
	}
	expected, actual := sha256.Sum256([]byte(s.cfg.ImportToken)), sha256.Sum256([]byte(value))
	if value == "" || subtle.ConstantTimeCompare(expected[:], actual[:]) != 1 {
		importFailure(c, 401, "IMPORT_UNAUTHORIZED")
		return false
	}

	return true
}

func (s *Server) importAppleServices(c *gin.Context) {
	if !s.authorizeAppleServices(c) {
		return
	}
	key := c.ClientIP()
	// An authenticated request is still not allowed to inject arbitrary fields,
	// query tokens, duplicate keys, multiple JSON values or compressed bodies.
	media, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || media != "application/json" || c.Request.URL.RawQuery != "" || c.GetHeader("Content-Encoding") != "" {
		importFailure(c, 400, "INVALID_IMPORT_REQUEST")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, importMaxBytes)
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var oversized *http.MaxBytesError
		if errors.As(err, &oversized) {
			importFailure(c, 413, "IMPORT_TOO_LARGE")
		} else {
			importFailure(c, 400, "INVALID_IMPORT_REQUEST")
		}
		return
	}
	var request account.ImportRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if !strictImportShape(raw) || !uniqueImportKeys(raw) || decoder.Decode(&request) != nil || request.NormalizeAndValidate() != nil {
		importFailure(c, 400, "INVALID_IMPORT_REQUEST")
		return
	}
	be, ok := s.be.(appleServicesImporter)
	if !ok {
		importFailure(c, 503, "IMPORT_UNAVAILABLE")
		return
	}
	select {
	case s.importSlots <- struct{}{}:
		defer func() { <-s.importSlots }()
	default:
		importFailure(c, 503, "IMPORT_BUSY")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	result, created, err := be.ImportAppleServices(ctx, request)
	if err != nil {
		// Never serialize/log the raw error, even if a backend violates the contract.
		code := "IMPORT_UNAVAILABLE"
		status := 503
		switch err {
		case account.ImportInvalid:
			code = string(account.ImportInvalid)
			status = 400
		case account.ImportConflict:
			code = string(account.ImportConflict)
			status = 409
		case account.ImportAppleFailed, account.ImportHMEFailed, account.ImportIMAPFailed:
			code = err.Error()
			status = 422
		case account.ImportStorageFailed:
			code = string(account.ImportStorageFailed)
		}
		importFailure(c, status, code)
		return
	}
	s.importLimiter.Success(key)
	status := 200
	if created {
		status = 201
	}
	c.JSON(status, gin.H{"success": true, "data": result})
}

func strictImportShape(raw []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	allowed := map[string]bool{"version": true, "source_id": true, "apple_id": true, "name": true, "icloud_email": true, "host": true, "cookies": true, "app_password": true}
	for key, value := range fields {
		if !allowed[key] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return false
		}
	}
	var cookies map[string]any
	if json.Unmarshal(fields["cookies"], &cookies) != nil {
		return false
	}
	for _, value := range cookies {
		if _, ok := value.(string); !ok {
			return false
		}
	}
	return true
}

// JSON's default last-key-wins behavior is undesirable for an identity-bearing
// protocol. Walk objects recursively, rejecting duplicates and trailing values.
func uniqueImportKeys(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() bool
	walk = func() bool {
		token, err := d.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return false
				}
				seen[name] = true
				if !walk() {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim('}')
		case '[':
			for d.More() {
				if !walk() {
					return false
				}
			}
			end, err := d.Token()
			return err == nil && end == json.Delim(']')
		}
		return false
	}
	if !walk() {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}

// The standard Gin logger/recovery can dump query strings or request headers.
// For this credential endpoint log only a fixed label and status; never errors,
// headers, bodies, URL, client-supplied IDs, or recovered panic values.
func importSafeLogging() gin.HandlerFunc {
	normal := gin.Logger()
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/integrations/appleservices") {
			c.Next()
			operation := "unknown"
			switch c.Request.URL.Path {
			case importPath:
				operation = "accounts"
			case "/api/integrations/appleservices/apple-login":
				operation = "apple_login"
			case "/api/integrations/appleservices/recovery/wait":
				operation = "recovery_wait"
			case "/api/integrations/appleservices/recovery/complete":
				operation = "recovery_complete"
			case "/api/integrations/appleservices/recovery/web-session":
				operation = "recovery_web_session"
			}
			log.Printf("appleservices operation=%s status=%d", operation, c.Writer.Status())
			return
		}
		normal(c)
	}
}
func importSafeRecovery() gin.HandlerFunc {
	normal := gin.Recovery()
	return func(c *gin.Context) {
		if strings.HasPrefix(c.Request.URL.Path, "/api/integrations/appleservices") {
			defer func() {
				if recover() != nil {
					importFailure(c, 503, "IMPORT_UNAVAILABLE")
				}
			}()
			c.Next()
			return
		}
		normal(c)
	}
}
