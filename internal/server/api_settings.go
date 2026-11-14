package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

type apiSettings struct {
	Enabled bool   `json:"enabled"`
	Token   string `json:"token"`
}

type apiSettingsStore struct {
	mu    sync.RWMutex
	value apiSettings
	path  string
}

func validAPIToken(token string) bool {
	if len(token) < 32 || len(token) > 512 {
		return false
	}
	for _, c := range token {
		if c < 33 || c > 126 {
			return false
		}
	}
	return true
}

func loadAPISettings(path, fallback string) (apiSettings, error) {
	value := apiSettings{Enabled: fallback != "", Token: fallback}
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return value, errors.New("读取 API 设置失败")
		}
		if err == nil {
			value = apiSettings{}
			if json.Unmarshal(raw, &value) != nil {
				return value, errors.New("API 设置文件格式无效")
			}
		}
	}
	if (value.Token != "" && !validAPIToken(value.Token)) || (value.Enabled && value.Token == "") {
		return value, errors.New("API Token 需为 32–512 个可见 ASCII 字符")
	}
	return value, nil
}

func (s *apiSettingsStore) snapshot() apiSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

// Caller holds mu. Persist before publishing, with atomic rename and mode 0600.
func (s *apiSettingsStore) save(value apiSettings) error {
	if s.path != "" {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		f, err := os.CreateTemp(filepath.Dir(s.path), ".api-settings-*")
		if err != nil {
			return err
		}
		defer os.Remove(f.Name())
		if _, err = f.Write(raw); err != nil {
			f.Close()
			return err
		}
		if err = f.Sync(); err != nil {
			f.Close()
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		if err = os.Rename(f.Name(), s.path); err != nil {
			return err
		}
	}
	s.value = value
	return nil
}

func settingsSummary(value apiSettings) gin.H {
	return gin.H{"enabled": value.Enabled, "has_token": value.Token != "", "base_path": "/api/v1"}
}

func (s *Server) getAPISettings(c *gin.Context) {
	ok(c, settingsSummary(s.apiSettings.snapshot()))
}

func (s *Server) revealAPIToken(c *gin.Context) {
	ok(c, gin.H{"token": s.apiSettings.snapshot().Token})
}

func (s *Server) updateAPISettings(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	var req struct {
		Enabled *bool   `json:"enabled"`
		Token   *string `json:"token"`
	}
	if c.ShouldBindJSON(&req) != nil || (req.Enabled == nil && req.Token == nil) {
		failCode(c, 400, "VALIDATION_ERROR", "请填写 API 设置")
		return
	}
	s.apiSettings.mu.Lock()
	defer s.apiSettings.mu.Unlock()
	next := s.apiSettings.value
	if req.Enabled != nil {
		next.Enabled = *req.Enabled
	}
	if req.Token != nil {
		if !validAPIToken(*req.Token) {
			failCode(c, 400, "VALIDATION_ERROR", "Token 需为 32–512 个可见 ASCII 字符，且不含空格")
			return
		}
		next.Token = *req.Token
	}
	if next.Enabled && strings.TrimSpace(next.Token) == "" {
		failCode(c, 400, "VALIDATION_ERROR", "请先生成或设置 Token")
		return
	}
	if err := s.apiSettings.save(next); err != nil {
		failCode(c, 500, "INTERNAL_ERROR", "保存 API 设置失败，原设置未变更")
		return
	}
	ok(c, settingsSummary(next))
}

func (s *Server) rotateAPIToken(c *gin.Context) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		failCode(c, 500, "INTERNAL_ERROR", "生成 Token 失败")
		return
	}
	s.apiSettings.mu.Lock()
	defer s.apiSettings.mu.Unlock()
	next := s.apiSettings.value
	next.Token = base64.RawURLEncoding.EncodeToString(raw)
	if err := s.apiSettings.save(next); err != nil {
		failCode(c, 500, "INTERNAL_ERROR", "保存 Token 失败，原 Token 未变更")
		return
	}
	// Rotating never silently enables a disabled service.
	ok(c, gin.H{"token": next.Token, "enabled": next.Enabled, "has_token": true, "base_path": "/api/v1"})
}
