package auth

import (
	"log/slog"
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/JonaWu05/DocNest/internal/authz"
)

// adminGuard 確認呼叫者為管理員；否則回 403 並中止。中介層只驗 JWT，管理權限一律在此再驗一次。
func (a *Auth) adminGuard(c *gin.Context) bool {
	if a.az.IsAdmin(authz.SubjectOf(c)) {
		return true
	}
	c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "需要管理員權限"})
	return false
}

// OverviewHandler 處理 GET /api/admin/overview：回傳帳號與群組的總覽（不含任何密碼 hash）。
func (a *Auth) OverviewHandler(c *gin.Context) {
	if !a.adminGuard(c) {
		return
	}

	locals := a.accounts.LocalUsernames()
	sort.Strings(locals)
	localOut := make([]gin.H, 0, len(locals))
	for _, u := range locals {
		localOut = append(localOut, gin.H{"username": u, "groups": a.az.GroupsOf("local:" + u)})
	}

	ids := a.accounts.DiscordIDs()
	sort.Strings(ids)
	discordOut := make([]gin.H, 0, len(ids))
	for _, id := range ids {
		discordOut = append(discordOut, gin.H{"id": id, "groups": a.az.GroupsOf("discord:" + id)})
	}

	c.JSON(http.StatusOK, gin.H{
		"perms_enabled": a.az.Enabled(),
		"admin_group":   authz.AdminGroup,
		"groups":        a.az.ListGroups(),
		"local":         localOut,
		"discord":       discordOut,
	})
}

// CreateUserHandler 處理 POST /api/admin/user/create：新增本地帳號。
func (a *Auth) CreateUserHandler(c *gin.Context) {
	if !a.adminGuard(c) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}
	if err := ValidateUsername(req.Username); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := a.accounts.CreateUser(req.Username, hash); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Info("管理員新增本地帳號", "by", c.GetString("username"), "user", req.Username)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// SetUserPasswordHandler 處理 POST /api/admin/user/password：管理員重設某本地帳號密碼。
func (a *Auth) SetUserPasswordHandler(c *gin.Context) {
	if !a.adminGuard(c) {
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := a.accounts.SetPassword(req.Username, hash); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Info("管理員重設帳號密碼", "by", c.GetString("username"), "user", req.Username)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// DeleteUserHandler 處理 POST /api/admin/user/delete：刪除本地帳號。
// 自我保護：不可刪除自己（避免把自己鎖在外面）。
func (a *Auth) DeleteUserHandler(c *gin.Context) {
	if !a.adminGuard(c) {
		return
	}
	var req struct {
		Username string `json:"username"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}
	if "local:"+req.Username == authz.SubjectOf(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不可刪除自己的帳號"})
		return
	}
	if err := a.accounts.DeleteUser(req.Username); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Info("管理員刪除本地帳號", "by", c.GetString("username"), "user", req.Username)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// AddDiscordHandler 處理 POST /api/admin/discord/add：加入 Discord 登入白名單。
func (a *Auth) AddDiscordHandler(c *gin.Context) {
	if !a.adminGuard(c) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}
	if err := a.accounts.AddDiscord(req.ID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Info("管理員新增 Discord 白名單", "by", c.GetString("username"), "id", req.ID)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// RemoveDiscordHandler 處理 POST /api/admin/discord/remove：移出 Discord 登入白名單。
// 自我保護：不可移除自己。
func (a *Auth) RemoveDiscordHandler(c *gin.Context) {
	if !a.adminGuard(c) {
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}
	if "discord:"+req.ID == authz.SubjectOf(c) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "不可移除自己"})
		return
	}
	if err := a.accounts.RemoveDiscord(req.ID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Info("管理員移除 Discord 白名單", "by", c.GetString("username"), "id", req.ID)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// SetGroupMemberHandler 處理 POST /api/admin/group/member：把身分鍵加入/移出既有群組。
// 自我保護：不可把自己移出管理員群組、不可移除最後一位管理員（避免鎖死）。
func (a *Auth) SetGroupMemberHandler(c *gin.Context) {
	if !a.adminGuard(c) {
		return
	}
	var req struct {
		Group   string `json:"group"`
		Subject string `json:"subject"`
		Add     bool   `json:"add"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}
	// 自我保護：移出管理員群組時的防鎖死檢查
	if req.Group == authz.AdminGroup && !req.Add {
		if req.Subject == authz.SubjectOf(c) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不可把自己移出管理員群組"})
			return
		}
		if len(a.az.AdminSubjects()) <= 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "不可移除最後一位管理員"})
			return
		}
	}
	if err := a.az.SetGroupMember(req.Group, req.Subject, req.Add); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Info("管理員調整群組成員", "by", c.GetString("username"), "group", req.Group, "subject", req.Subject, "add", req.Add)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ChangeOwnPasswordHandler 處理 POST /api/me/password：登入者自助修改自己的密碼（僅本地帳號）。
// 需驗舊密碼。Discord 登入者無密碼，一律拒絕。
func (a *Auth) ChangeOwnPasswordHandler(c *gin.Context) {
	if c.GetString("login_type") != "local" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "僅本地帳號可修改密碼"})
		return
	}
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "請求格式錯誤"})
		return
	}
	username := c.GetString("username")
	hash, ok := a.accounts.Lookup(username)
	if !ok || bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.OldPassword)) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "舊密碼錯誤"})
		return
	}
	newHash, err := HashPassword(req.NewPassword)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := a.accounts.SetPassword(username, newHash); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	slog.Info("使用者自助修改密碼", "user", username)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
