package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"icloud-hme/internal/account"
	"icloud-hme/internal/hme"
)

// One service process owns this durable ledger. Allocations and imports share
// its mutex, so the same caller cannot receive an address twice. Different
// callers have independent usage histories and may share an address.
// Claims are never released on registration failure or token rotation.
type aliasUsageStore struct {
	mu     sync.Mutex
	path   string
	claims map[string]aliasAllocation
}

type aliasAllocation struct {
	CallerID    string  `json:"caller_id"`
	AccountID   string  `json:"account_id"`
	Email       string  `json:"email"`
	AnonymousID string  `json:"anonymousId,omitempty"`
	Label       string  `json:"label,omitempty"`
	CreatedAt   string  `json:"created_at,omitempty"`
	Source      string  `json:"source"`
	AllocatedAt float64 `json:"allocated_at"`
	RequestID   string  `json:"request_id,omitempty"`
	Fingerprint string  `json:"-"`
}

type savedAliasClaim struct {
	aliasAllocation
	Fingerprint string `json:"request_fingerprint,omitempty"`
}

type aliasUsageDocument struct {
	Version int                        `json:"version"`
	Claims  map[string]savedAliasClaim `json:"claims"`
}

// Empty/default keeps the old request fingerprint and ledger key unchanged.
// Caller IDs are grouping labels, not separate authentication credentials.
func normalizeCallerID(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 64 {
		return "", errors.New("caller_id 最长 64 字符")
	}
	for _, c := range value {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.ContainsRune("-_.", c)) {
			return "", errors.New("caller_id 仅支持字母、数字、连字符、下划线和点")
		}
	}
	if value == "default" {
		value = ""
	}
	return value, nil
}

func callerGroup(value string) string {
	if value == "" {
		return "default"
	}
	return value
}

func aliasClaimKey(callerID, value string) string {
	if callerID == "" || callerID == "default" {
		return value
	}
	// The prefix cannot collide with a default email (exactly one @) or a
	// default request_id (which forbids @), even when a local part contains |.
	return "@" + callerID + "|" + value
}

func normalizeAliasAddress(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || strings.Count(value, "@") != 1 ||
		!strings.Contains(strings.SplitN(value, "@", 2)[1], ".") {
		return ""
	}
	return value
}

func loadAliasUsage(path string) (*aliasUsageStore, error) {
	store := &aliasUsageStore{path: path, claims: make(map[string]aliasAllocation)}
	if path == "" {
		return store, nil // Test-only in-memory store; main always sets a path.
	}
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, errors.New("读取别名占用记录失败")
	}
	var document aliasUsageDocument
	if json.Unmarshal(body, &document) != nil || (document.Version != 1 && document.Version != 2) || document.Claims == nil {
		return nil, errors.New("别名占用记录格式错误，请恢复备份；现有记录不会被自动清空")
	}
	requests := make(map[string]bool)
	for key, saved := range document.Claims {
		claim := saved.aliasAllocation
		claim.Fingerprint = saved.Fingerprint
		caller, err := normalizeCallerID(claim.CallerID)
		if err != nil || (document.Version == 1 && caller != "") ||
			(document.Version == 2 && claim.CallerID != callerGroup(caller)) {
			return nil, errors.New("别名占用记录调用商校验失败，请恢复备份")
		}
		claim.CallerID = callerGroup(caller)
		requestKey := aliasClaimKey(claim.CallerID, claim.RequestID)
		if normalizeAliasAddress(claim.Email) != claim.Email || claim.Email == "" || key != aliasClaimKey(claim.CallerID, claim.Email) || claim.AllocatedAt <= 0 ||
			(claim.RequestID != "" && (requests[requestKey] || claim.Fingerprint == "")) {
			return nil, errors.New("别名占用记录校验失败，请恢复备份")
		}
		if claim.RequestID != "" {
			requests[requestKey] = true
		}
		store.claims[key] = claim
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, errors.New("设置别名占用记录权限失败")
	}
	return store, nil
}

// Caller holds mu. Persist the next snapshot before making it visible or
// returning an address. Atomic rename prevents a partial ledger on restart.
func (store *aliasUsageStore) commit(next map[string]aliasAllocation) error {
	if store.path != "" {
		document := aliasUsageDocument{Version: 2, Claims: make(map[string]savedAliasClaim, len(next))}
		for email, claim := range next {
			document.Claims[email] = savedAliasClaim{aliasAllocation: claim, Fingerprint: claim.Fingerprint}
		}
		body, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return err
		}
		directory := filepath.Dir(store.path)
		if err := os.MkdirAll(directory, 0700); err != nil {
			return err
		}
		file, err := os.CreateTemp(directory, ".alias-usage-*")
		if err != nil {
			return err
		}
		defer os.Remove(file.Name())
		if err = file.Chmod(0600); err == nil {
			_, err = file.Write(append(body, '\n'))
		}
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
		if err := os.Rename(file.Name(), store.path); err != nil {
			return err
		}
		if dir, err := os.Open(directory); err == nil {
			_ = dir.Sync()
			_ = dir.Close()
		}
	}
	store.claims = next
	return nil
}

func (store *aliasUsageStore) copyClaims() map[string]aliasAllocation {
	next := make(map[string]aliasAllocation, len(store.claims)+1)
	for email, claim := range store.claims {
		next[email] = claim
	}
	return next
}

type allocateAliasRequest struct {
	AccountID     string   `json:"account_id"`
	Label         string   `json:"label"`
	LabelPrefix   string   `json:"label_prefix"`
	AutoCreate    *bool    `json:"auto_create"`
	MaxAliases    *int     `json:"max_aliases"`
	ExcludeEmails []string `json:"exclude_emails"`
	RequestID     string   `json:"request_id"`
	CallerID      string   `json:"caller_id,omitempty"`
}

func normalizeAllocationRequest(req *allocateAliasRequest) error {
	caller, err := normalizeCallerID(req.CallerID)
	if err != nil {
		return err
	}
	req.CallerID = caller
	req.AccountID = strings.TrimSpace(req.AccountID)
	req.Label = strings.TrimSpace(req.Label)
	req.LabelPrefix = strings.TrimSpace(req.LabelPrefix)
	if len(req.AccountID) > 256 || len([]rune(req.Label)) > 200 || len([]rune(req.LabelPrefix)) > 150 || len(req.ExcludeEmails) > 10000 {
		return errors.New("账号、标签或排除列表超出长度限制")
	}
	if req.AutoCreate == nil {
		value := true
		req.AutoCreate = &value
	}
	if req.MaxAliases == nil {
		value := MaxAliases
		req.MaxAliases = &value
	}
	if *req.MaxAliases < 1 || *req.MaxAliases > MaxAliases {
		return errors.New("max_aliases 需为 1-750 的整数")
	}
	if req.RequestID == "" {
		req.RequestID = uuid.NewString()
	}
	if len(req.RequestID) > 128 {
		return errors.New("request_id 最长 128 字符")
	}
	for _, character := range req.RequestID {
		if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("-_.", character)) {
			return errors.New("request_id 仅支持字母、数字、连字符、下划线和点")
		}
	}
	seen := make(map[string]bool)
	excluded := make([]string, 0, len(req.ExcludeEmails))
	for _, email := range req.ExcludeEmails {
		address := normalizeAliasAddress(email)
		if address == "" {
			return errors.New("exclude_emails 含无效邮箱")
		}
		if !seen[address] {
			seen[address] = true
			excluded = append(excluded, address)
		}
	}
	sort.Strings(excluded)
	req.ExcludeEmails = excluded
	return nil
}

func usageWriteError() error {
	return &BackendError{Status: http.StatusInternalServerError, Code: "ALIAS_USAGE_WRITE_FAILED", Message: "保存别名占用记录失败，本次未返回可用邮箱"}
}

func (s *Server) allocateAlias(ctx context.Context, req allocateAliasRequest) (*aliasAllocation, error) {
	store := s.aliasUsage
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, _ := json.Marshal(req)
	digest := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(digest[:])
	callerID := callerGroup(req.CallerID)
	for _, claim := range store.claims {
		if callerGroup(claim.CallerID) == callerID && claim.RequestID == req.RequestID {
			if claim.Fingerprint != fingerprint {
				return nil, &BackendError{Status: http.StatusConflict, Code: "REQUEST_ID_CONFLICT", Message: "该 request_id 已用于不同的分配参数"}
			}
			return &claim, nil
		}
	}
	allAccounts := s.be.ListAccounts()
	accounts := make([]account.Summary, 0, len(allAccounts))
	primaries := make(map[string]bool)
	for _, acc := range allAccounts {
		for _, value := range []string{acc.RealEmail, acc.ICloudEmail} {
			if address := normalizeAliasAddress(value); address != "" {
				primaries[address] = true
			}
		}
		if req.AccountID == "" || acc.ID == req.AccountID {
			accounts = append(accounts, acc)
		}
	}
	if len(accounts) == 0 {
		return nil, &BackendError{Status: http.StatusNotFound, Code: "ACCOUNT_NOT_FOUND", Message: "没有匹配的已配置账号"}
	}
	// Import legacy reservations before contacting Apple. Even an allocation
	// failure must not make these historical addresses available again.
	next := store.copyClaims()
	for _, email := range req.ExcludeEmails {
		key := aliasClaimKey(callerID, email)
		if _, exists := next[key]; !exists {
			next[key] = aliasAllocation{CallerID: callerID, Email: email, AccountID: req.AccountID, Source: "imported", AllocatedAt: float64(time.Now().UnixMilli()) / 1000}
		}
	}
	if len(next) != len(store.claims) {
		if store.commit(next) != nil {
			return nil, usageWriteError()
		}
	}
	claim := func(accID string, alias hme.Alias, source, createdAt string) (*aliasAllocation, error) {
		email := normalizeAliasAddress(alias.Email)
		if email == "" || primaries[email] {
			return nil, nil
		}
		key := aliasClaimKey(callerID, email)
		if _, exists := store.claims[key]; exists {
			return nil, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result := aliasAllocation{CallerID: callerID, AccountID: accID, Email: email, AnonymousID: alias.AnonymousID,
			Label: alias.Label, CreatedAt: createdAt, Source: source, AllocatedAt: float64(time.Now().UnixMilli()) / 1000,
			RequestID: req.RequestID, Fingerprint: fingerprint}
		next := store.copyClaims()
		next[key] = result
		if store.commit(next) != nil {
			return nil, usageWriteError()
		}
		return &result, nil
	}
	type pool struct {
		account account.Summary
		aliases []hme.Alias
	}
	pools := make([]pool, 0, len(accounts))
	var listErr error
	// Examine existing addresses across all selected accounts before creating.
	for _, acc := range accounts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		aliases, err := s.be.ListAliases(acc.ID)
		if err != nil {
			if listErr == nil {
				listErr = err
			}
			continue
		}
		pools = append(pools, pool{acc, aliases})
		for _, alias := range aliases {
			if !alias.Active || alias.AnonymousID == "" {
				continue
			}
			result, err := claim(acc.ID, alias, "existing", alias.CreatedAt)
			if result != nil || err != nil {
				return result, err
			}
		}
	}
	if listErr != nil {
		return nil, listErr // An unreadable pool is not an empty pool.
	}
	if !*req.AutoCreate {
		return nil, &BackendError{Status: http.StatusConflict, Code: "NO_AVAILABLE_ALIAS", Message: "当前调用商没有未使用别名，自动创建已关闭"}
	}
	for _, pool := range pools {
		if len(pool.aliases) >= *req.MaxAliases {
			continue
		}
		label := req.Label
		if label == "" {
			prefix := req.LabelPrefix
			if prefix == "" {
				prefix = "api"
			}
			label = prefix + "-" + uuid.NewString()[:12]
		}
		created, _, err := s.createAliasWithinCustomLimit(ctx, pool.account.ID, label, *req.MaxAliases)
		if err != nil {
			var be *BackendError
			if errors.As(err, &be) && be.Code == "ALIAS_LIMIT_REACHED" {
				continue
			}
			return nil, err
		}
		result, err := claim(pool.account.ID, hme.Alias{Email: created.Email, Label: created.Label}, "created", created.CreatedAt)
		if err != nil {
			return nil, err
		}
		if result == nil {
			return nil, &BackendError{Status: http.StatusBadGateway, Code: "UPSTREAM_FAILURE", Message: "创建结果不是可用的新别名，主邮箱不会被使用"}
		}
		return result, nil
	}
	return nil, &BackendError{Status: http.StatusConflict, Code: "ALIAS_LIMIT_REACHED", Message: fmt.Sprintf("当前调用商没有未使用别名，主号已达 %d 个别名上限（含已停用别名）", *req.MaxAliases)}
}

func (s *Server) allocateAliasHandler(c *gin.Context) {
	var req allocateAliasRequest
	if c.ShouldBindJSON(&req) != nil {
		failCode(c, 400, "VALIDATION_ERROR", "参数错误")
		return
	}
	if err := normalizeAllocationRequest(&req); err != nil {
		failCode(c, 400, "VALIDATION_ERROR", err.Error())
		return
	}
	result, err := s.allocateAlias(c.Request.Context(), req)
	if err != nil {
		backendFail(c, err)
		return
	}
	ok(c, result)
}

func (s *Server) aliasUsageHandler(c *gin.Context) {
	caller, err := normalizeCallerID(c.Query("caller_id"))
	if err != nil {
		failCode(c, 400, "VALIDATION_ERROR", err.Error())
		return
	}
	_, scoped := c.Request.URL.Query()["caller_id"]
	callerID := callerGroup(caller)
	s.aliasUsage.mu.Lock()
	defer s.aliasUsage.mu.Unlock()
	counts := make(map[string]int)
	emails := make(map[string]bool)
	used := 0
	for _, claim := range s.aliasUsage.claims {
		group := callerGroup(claim.CallerID)
		if scoped && group != callerID {
			continue
		}
		counts[group]++
		emails[claim.Email] = true
		used++
	}
	groups := make([]string, 0, len(counts))
	for group := range counts {
		groups = append(groups, group)
	}
	sort.Strings(groups)
	callers := make([]gin.H, 0, len(groups))
	for _, group := range groups {
		callers = append(callers, gin.H{"caller_id": group, "used_count": counts[group]})
	}
	data := gin.H{"used_count": used, "unique_email_count": len(emails), "callers": callers}
	if scoped {
		data["caller_id"] = callerID
	}
	ok(c, data)
}

func (s *Server) importAliasUsageHandler(c *gin.Context) {
	var req struct {
		CallerID string `json:"caller_id"`
		Entries  []struct {
			AccountID string  `json:"account_id"`
			Email     string  `json:"email"`
			ClaimedAt float64 `json:"claimed_at"`
		} `json:"entries"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Entries == nil || len(req.Entries) > 10000 {
		failCode(c, 400, "VALIDATION_ERROR", "entries 需为最多 10000 条占用记录的数组")
		return
	}
	caller, err := normalizeCallerID(req.CallerID)
	if err != nil {
		failCode(c, 400, "VALIDATION_ERROR", err.Error())
		return
	}
	callerID := callerGroup(caller)
	for i := range req.Entries {
		entry := &req.Entries[i]
		entry.Email = normalizeAliasAddress(entry.Email)
		entry.AccountID = strings.TrimSpace(entry.AccountID)
		if entry.Email == "" || len(entry.AccountID) > 256 || entry.ClaimedAt < 0 || math.IsNaN(entry.ClaimedAt) || math.IsInf(entry.ClaimedAt, 0) {
			failCode(c, 400, "VALIDATION_ERROR", "占用记录包含无效邮箱、账号或时间")
			return
		}
	}
	store := s.aliasUsage
	store.mu.Lock()
	defer store.mu.Unlock()
	next := store.copyClaims()
	for _, entry := range req.Entries {
		key := aliasClaimKey(callerID, entry.Email)
		if _, exists := next[key]; exists {
			continue
		}
		at := entry.ClaimedAt
		if at == 0 {
			at = float64(time.Now().UnixMilli()) / 1000
		}
		next[key] = aliasAllocation{CallerID: callerID, AccountID: entry.AccountID, Email: entry.Email, Source: "imported", AllocatedAt: at}
	}
	added := len(next) - len(store.claims)
	if added != 0 && store.commit(next) != nil {
		backendFail(c, usageWriteError())
		return
	}
	callerUsed := 0
	for _, claim := range store.claims {
		if callerGroup(claim.CallerID) == callerID {
			callerUsed++
		}
	}
	ok(c, gin.H{"caller_id": callerID, "imported": added, "used_count": len(store.claims), "caller_used_count": callerUsed})
}
