package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"icloud-hme/internal/account"
)

type appleServicesLoginBackend interface {
	ImportAppleLogin(context.Context, account.ImportAppleLoginRequest) (account.ImportAppleLoginResult, error)
}

func (b *managerBackend) ImportAppleLogin(ctx context.Context, r account.ImportAppleLoginRequest) (account.ImportAppleLoginResult, error) {
	return b.mgr.ImportAppleLogin(ctx, r)
}
func (s *Server) importAppleLogin(c *gin.Context) {
	if !s.authorizeAppleServices(c) {
		return
	}
	content, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || content != "application/json" || c.Request.URL.RawQuery != "" || c.GetHeader("Content-Encoding") != "" {
		importFailure(c, 400, "INVALID_IMPORT_REQUEST")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 8192))
	defer clear(raw)
	if err != nil {
		importFailure(c, 400, "INVALID_IMPORT_REQUEST")
		return
	}
	var req account.ImportAppleLoginRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if !uniqueImportKeys(raw) || decoder.Decode(&req) != nil || req.Validate() != nil {
		importFailure(c, 400, "INVALID_IMPORT_REQUEST")
		return
	}
	backend, ok := s.be.(appleServicesLoginBackend)
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
	ctx, cancel := context.WithTimeout(c.Request.Context(), 110*time.Second)
	defer cancel()
	result, err := backend.ImportAppleLogin(ctx, req)
	if err != nil {
		switch err {
		case account.ImportInvalid:
			importFailure(c, 400, "INVALID_IMPORT_REQUEST")
		case account.ImportConflict:
			importFailure(c, 409, "ACCOUNT_IDENTITY_CONFLICT")
		case account.ImportUnavailable:
			importFailure(c, 503, "IMPORT_BUSY")
		case account.ImportStorageFailed:
			importFailure(c, 503, "IMPORT_STORAGE_FAILED")
		default:
			mapped := mapAppleError(err)
			importFailure(c, mapped.Status, mapped.Code)
		}
		return
	}
	if result.State == "ready" {
		s.acceptRecoverySession(req.SourceID)
	}
	s.importLimiter.Success(c.ClientIP())
	c.JSON(200, gin.H{"success": true, "data": result})
}
