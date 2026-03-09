package admin

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

type DataManagementHandler struct {
	dataManagementService dataManagementService
	pgBackupService       *service.PostgresBackupService
}

func NewDataManagementHandler(dataManagementService *service.DataManagementService, pgBackupService *service.PostgresBackupService) *DataManagementHandler {
	return &DataManagementHandler{dataManagementService: dataManagementService, pgBackupService: pgBackupService}
}

type dataManagementService interface {
	GetConfig(ctx context.Context) (service.DataManagementConfig, error)
	UpdateConfig(ctx context.Context, cfg service.DataManagementConfig) (service.DataManagementConfig, error)
	ValidateS3(ctx context.Context, cfg service.DataManagementS3Config) (service.DataManagementTestS3Result, error)
	CreateBackupJob(ctx context.Context, input service.DataManagementCreateBackupJobInput) (service.DataManagementBackupJob, error)
	ListSourceProfiles(ctx context.Context, sourceType string) ([]service.DataManagementSourceProfile, error)
	CreateSourceProfile(ctx context.Context, input service.DataManagementCreateSourceProfileInput) (service.DataManagementSourceProfile, error)
	UpdateSourceProfile(ctx context.Context, input service.DataManagementUpdateSourceProfileInput) (service.DataManagementSourceProfile, error)
	DeleteSourceProfile(ctx context.Context, sourceType, profileID string) error
	SetActiveSourceProfile(ctx context.Context, sourceType, profileID string) (service.DataManagementSourceProfile, error)
	ListS3Profiles(ctx context.Context) ([]service.DataManagementS3Profile, error)
	CreateS3Profile(ctx context.Context, input service.DataManagementCreateS3ProfileInput) (service.DataManagementS3Profile, error)
	UpdateS3Profile(ctx context.Context, input service.DataManagementUpdateS3ProfileInput) (service.DataManagementS3Profile, error)
	DeleteS3Profile(ctx context.Context, profileID string) error
	SetActiveS3Profile(ctx context.Context, profileID string) (service.DataManagementS3Profile, error)
	ListBackupJobs(ctx context.Context, input service.DataManagementListBackupJobsInput) (service.DataManagementListBackupJobsResult, error)
	GetBackupJob(ctx context.Context, jobID string) (service.DataManagementBackupJob, error)
	EnsureAgentEnabled(ctx context.Context) error
	GetAgentHealth(ctx context.Context) service.DataManagementAgentHealth
}

type TestS3ConnectionRequest struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region" binding:"required"`
	Bucket          string `json:"bucket" binding:"required"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"`
	ForcePathStyle  bool   `json:"force_path_style"`
	UseSSL          bool   `json:"use_ssl"`
}

type CreateBackupJobRequest struct {
	BackupType     string `json:"backup_type" binding:"required,oneof=postgres redis full"`
	UploadToS3     bool   `json:"upload_to_s3"`
	S3ProfileID    string `json:"s3_profile_id"`
	PostgresID     string `json:"postgres_profile_id"`
	RedisID        string `json:"redis_profile_id"`
	IdempotencyKey string `json:"idempotency_key"`
}

type CreateSourceProfileRequest struct {
	ProfileID string                             `json:"profile_id" binding:"required"`
	Name      string                             `json:"name" binding:"required"`
	Config    service.DataManagementSourceConfig `json:"config" binding:"required"`
	SetActive bool                               `json:"set_active"`
}

type UpdateSourceProfileRequest struct {
	Name   string                             `json:"name" binding:"required"`
	Config service.DataManagementSourceConfig `json:"config" binding:"required"`
}

type CreateS3ProfileRequest struct {
	ProfileID       string `json:"profile_id" binding:"required"`
	Name            string `json:"name" binding:"required"`
	Enabled         bool   `json:"enabled"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"`
	ForcePathStyle  bool   `json:"force_path_style"`
	UseSSL          bool   `json:"use_ssl"`
	SetActive       bool   `json:"set_active"`
}

type UpdateS3ProfileRequest struct {
	Name            string `json:"name" binding:"required"`
	Enabled         bool   `json:"enabled"`
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Prefix          string `json:"prefix"`
	ForcePathStyle  bool   `json:"force_path_style"`
	UseSSL          bool   `json:"use_ssl"`
}

func (h *DataManagementHandler) GetAgentHealth(c *gin.Context) {
	health := h.getAgentHealth(c)
	payload := gin.H{
		"enabled":     health.Enabled,
		"reason":      health.Reason,
		"socket_path": health.SocketPath,
	}
	if health.Agent != nil {
		payload["agent"] = gin.H{
			"status":         health.Agent.Status,
			"version":        health.Agent.Version,
			"uptime_seconds": health.Agent.UptimeSeconds,
		}
	}
	response.Success(c, payload)
}

func (h *DataManagementHandler) GetConfig(c *gin.Context) {
	if !h.requireAgentEnabled(c) {
		return
	}
	cfg, err := h.dataManagementService.GetConfig(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

func (h *DataManagementHandler) UpdateConfig(c *gin.Context) {
	var req service.DataManagementConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	cfg, err := h.dataManagementService.UpdateConfig(c.Request.Context(), req)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, cfg)
}

func (h *DataManagementHandler) TestS3(c *gin.Context) {
	var req TestS3ConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	result, err := h.dataManagementService.ValidateS3(c.Request.Context(), service.DataManagementS3Config{
		Enabled:         true,
		Endpoint:        req.Endpoint,
		Region:          req.Region,
		Bucket:          req.Bucket,
		AccessKeyID:     req.AccessKeyID,
		SecretAccessKey: req.SecretAccessKey,
		Prefix:          req.Prefix,
		ForcePathStyle:  req.ForcePathStyle,
		UseSSL:          req.UseSSL,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"ok": result.OK, "message": result.Message})
}

func (h *DataManagementHandler) CreateBackupJob(c *gin.Context) {
	var req CreateBackupJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	req.IdempotencyKey = normalizeBackupIdempotencyKey(c.GetHeader("X-Idempotency-Key"), req.IdempotencyKey)
	if !h.requireAgentEnabled(c) {
		return
	}

	triggeredBy := "admin:unknown"
	if subject, ok := middleware2.GetAuthSubjectFromContext(c); ok {
		triggeredBy = "admin:" + strconv.FormatInt(subject.UserID, 10)
	}
	job, err := h.dataManagementService.CreateBackupJob(c.Request.Context(), service.DataManagementCreateBackupJobInput{
		BackupType:     req.BackupType,
		UploadToS3:     req.UploadToS3,
		S3ProfileID:    req.S3ProfileID,
		PostgresID:     req.PostgresID,
		RedisID:        req.RedisID,
		TriggeredBy:    triggeredBy,
		IdempotencyKey: req.IdempotencyKey,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"job_id": job.JobID, "status": job.Status})
}

func (h *DataManagementHandler) ListSourceProfiles(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType == "" {
		response.BadRequest(c, "Invalid source_type")
		return
	}
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	items, err := h.dataManagementService.ListSourceProfiles(c.Request.Context(), sourceType)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

func (h *DataManagementHandler) CreateSourceProfile(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}

	var req CreateSourceProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	profile, err := h.dataManagementService.CreateSourceProfile(c.Request.Context(), service.DataManagementCreateSourceProfileInput{
		SourceType: sourceType,
		ProfileID:  req.ProfileID,
		Name:       req.Name,
		Config:     req.Config,
		SetActive:  req.SetActive,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, profile)
}

func (h *DataManagementHandler) UpdateSourceProfile(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	var req UpdateSourceProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	profile, err := h.dataManagementService.UpdateSourceProfile(c.Request.Context(), service.DataManagementUpdateSourceProfileInput{
		SourceType: sourceType,
		ProfileID:  profileID,
		Name:       req.Name,
		Config:     req.Config,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, profile)
}

func (h *DataManagementHandler) DeleteSourceProfile(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	if err := h.dataManagementService.DeleteSourceProfile(c.Request.Context(), sourceType, profileID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *DataManagementHandler) SetActiveSourceProfile(c *gin.Context) {
	sourceType := strings.TrimSpace(c.Param("source_type"))
	if sourceType != "postgres" && sourceType != "redis" {
		response.BadRequest(c, "source_type must be postgres or redis")
		return
	}
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	profile, err := h.dataManagementService.SetActiveSourceProfile(c.Request.Context(), sourceType, profileID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, profile)
}

func (h *DataManagementHandler) ListS3Profiles(c *gin.Context) {
	if !h.requireAgentEnabled(c) {
		return
	}

	items, err := h.dataManagementService.ListS3Profiles(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"items": items})
}

func (h *DataManagementHandler) CreateS3Profile(c *gin.Context) {
	var req CreateS3ProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}

	profile, err := h.dataManagementService.CreateS3Profile(c.Request.Context(), service.DataManagementCreateS3ProfileInput{
		ProfileID: req.ProfileID,
		Name:      req.Name,
		SetActive: req.SetActive,
		S3: service.DataManagementS3Config{
			Enabled:         req.Enabled,
			Endpoint:        req.Endpoint,
			Region:          req.Region,
			Bucket:          req.Bucket,
			AccessKeyID:     req.AccessKeyID,
			SecretAccessKey: req.SecretAccessKey,
			Prefix:          req.Prefix,
			ForcePathStyle:  req.ForcePathStyle,
			UseSSL:          req.UseSSL,
		},
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, profile)
}

func (h *DataManagementHandler) UpdateS3Profile(c *gin.Context) {
	var req UpdateS3ProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}

	profile, err := h.dataManagementService.UpdateS3Profile(c.Request.Context(), service.DataManagementUpdateS3ProfileInput{
		ProfileID: profileID,
		Name:      req.Name,
		S3: service.DataManagementS3Config{
			Enabled:         req.Enabled,
			Endpoint:        req.Endpoint,
			Region:          req.Region,
			Bucket:          req.Bucket,
			AccessKeyID:     req.AccessKeyID,
			SecretAccessKey: req.SecretAccessKey,
			Prefix:          req.Prefix,
			ForcePathStyle:  req.ForcePathStyle,
			UseSSL:          req.UseSSL,
		},
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, profile)
}

func (h *DataManagementHandler) DeleteS3Profile(c *gin.Context) {
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	if err := h.dataManagementService.DeleteS3Profile(c.Request.Context(), profileID); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

func (h *DataManagementHandler) SetActiveS3Profile(c *gin.Context) {
	profileID := strings.TrimSpace(c.Param("profile_id"))
	if profileID == "" {
		response.BadRequest(c, "Invalid profile_id")
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	profile, err := h.dataManagementService.SetActiveS3Profile(c.Request.Context(), profileID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, profile)
}

func (h *DataManagementHandler) ListBackupJobs(c *gin.Context) {
	if !h.requireAgentEnabled(c) {
		return
	}

	pageSize := int32(20)
	if raw := strings.TrimSpace(c.Query("page_size")); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil || v <= 0 {
			response.BadRequest(c, "Invalid page_size")
			return
		}
		pageSize = int32(v)
	}

	result, err := h.dataManagementService.ListBackupJobs(c.Request.Context(), service.DataManagementListBackupJobsInput{
		PageSize:   pageSize,
		PageToken:  c.Query("page_token"),
		Status:     c.Query("status"),
		BackupType: c.Query("backup_type"),
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *DataManagementHandler) GetBackupJob(c *gin.Context) {
	jobID := strings.TrimSpace(c.Param("job_id"))
	if jobID == "" {
		response.BadRequest(c, "Invalid backup job ID")
		return
	}

	if !h.requireAgentEnabled(c) {
		return
	}
	job, err := h.dataManagementService.GetBackupJob(c.Request.Context(), jobID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, job)
}

func (h *DataManagementHandler) requireAgentEnabled(c *gin.Context) bool {
	if h.dataManagementService == nil {
		err := infraerrors.ServiceUnavailable(
			service.DataManagementAgentUnavailableReason,
			"data management agent service is not configured",
		).WithMetadata(map[string]string{"socket_path": service.DefaultDataManagementAgentSocketPath})
		response.ErrorFrom(c, err)
		return false
	}

	if err := h.dataManagementService.EnsureAgentEnabled(c.Request.Context()); err != nil {
		response.ErrorFrom(c, err)
		return false
	}

	return true
}

func (h *DataManagementHandler) getAgentHealth(c *gin.Context) service.DataManagementAgentHealth {
	if h.dataManagementService == nil {
		return service.DataManagementAgentHealth{
			Enabled:    false,
			Reason:     service.DataManagementAgentUnavailableReason,
			SocketPath: service.DefaultDataManagementAgentSocketPath,
		}
	}
	return h.dataManagementService.GetAgentHealth(c.Request.Context())
}

func (h *DataManagementHandler) GetPostgresInfo(c *gin.Context) {
	info := h.pgBackupService.GetInfo()
	toolsOK := true
	toolsError := ""
	if err := h.pgBackupService.CheckTools(); err != nil {
		toolsOK = false
		toolsError = err.Error()
	}
	response.Success(c, gin.H{
		"host":        info.Host,
		"port":        info.Port,
		"dbname":      info.DBName,
		"user":        info.User,
		"sslmode":     info.SSLMode,
		"tools_ok":    toolsOK,
		"tools_error": toolsError,
	})
}

func (h *DataManagementHandler) ExportPostgres(c *gin.Context) {
	if err := h.pgBackupService.CheckTools(); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "pg_dump is not available: "+err.Error())
		return
	}

	filename := h.pgBackupService.ExportFilename()
	c.Header("Content-Disposition", "attachment; filename=\""+filename+"\"")
	c.Header("Content-Type", "application/octet-stream")
	c.Status(http.StatusOK)

	if err := h.pgBackupService.Export(c.Request.Context(), c.Writer); err != nil {
		c.String(http.StatusInternalServerError, "Export failed: "+err.Error())
		return
	}
}

func (h *DataManagementHandler) RestorePostgres(c *gin.Context) {
	if err := h.pgBackupService.CheckTools(); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "pg_restore is not available: "+err.Error())
		return
	}

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		response.BadRequest(c, "Missing file: "+err.Error())
		return
	}
	defer func() { _ = file.Close() }()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".dump") {
		response.BadRequest(c, "Only .dump files (pg_dump custom format) are accepted")
		return
	}

	confirm := strings.TrimSpace(c.PostForm("confirm"))
	dbName := h.pgBackupService.GetInfo().DBName
	expected := "RESTORE " + dbName
	if confirm != expected {
		response.BadRequest(c, "Confirmation text must be exactly: "+expected)
		return
	}

	if err := h.pgBackupService.Restore(c.Request.Context(), io.Reader(file)); err != nil {
		response.InternalError(c, "Restore failed: "+err.Error())
		return
	}

	response.Success(c, gin.H{
		"restored":     true,
		"need_restart": true,
		"message":      "Database restored successfully. Please restart the service.",
	})
}

func (h *DataManagementHandler) InitRestoreUpload(c *gin.Context) {
	if err := h.pgBackupService.CheckTools(); err != nil {
		response.Error(c, http.StatusServiceUnavailable, "pg_restore is not available: "+err.Error())
		return
	}

	var req struct {
		Filename  string `json:"filename" binding:"required"`
		TotalSize int64  `json:"total_size" binding:"required,gt=0"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	if !strings.HasSuffix(strings.ToLower(req.Filename), ".dump") {
		response.BadRequest(c, "Only .dump files (pg_dump custom format) are accepted")
		return
	}

	uploadID, chunkCount, err := h.pgBackupService.InitUpload(req.Filename, req.TotalSize)
	if err != nil {
		response.InternalError(c, "Failed to init upload: "+err.Error())
		return
	}

	response.Success(c, gin.H{
		"upload_id":   uploadID,
		"chunk_size":  service.ChunkSize,
		"chunk_count": chunkCount,
	})
}

func (h *DataManagementHandler) UploadRestoreChunk(c *gin.Context) {
	uploadID := strings.TrimSpace(c.Param("upload_id"))
	if uploadID == "" {
		response.BadRequest(c, "Missing upload_id")
		return
	}
	indexStr := strings.TrimSpace(c.PostForm("index"))
	if indexStr == "" {
		indexStr = strings.TrimSpace(c.Query("index"))
	}
	index, err := strconv.Atoi(indexStr)
	if err != nil {
		response.BadRequest(c, "Invalid chunk index")
		return
	}

	file, _, err := c.Request.FormFile("chunk")
	if err != nil {
		response.BadRequest(c, "Missing chunk file: "+err.Error())
		return
	}
	defer func() { _ = file.Close() }()

	allDone, err := h.pgBackupService.SaveChunk(uploadID, index, file)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}

	received, total, _ := h.pgBackupService.GetUploadProgress(uploadID)
	response.Success(c, gin.H{
		"upload_id": uploadID,
		"index":     index,
		"received":  received,
		"total":     total,
		"complete":  allDone,
	})
}

func (h *DataManagementHandler) CompleteRestoreUpload(c *gin.Context) {
	uploadID := strings.TrimSpace(c.Param("upload_id"))
	if uploadID == "" {
		response.BadRequest(c, "Missing upload_id")
		return
	}

	var req struct {
		Confirm string `json:"confirm" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	if err := h.pgBackupService.CompleteUpload(c.Request.Context(), uploadID, req.Confirm); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "confirmation text") {
			response.BadRequest(c, errMsg)
			return
		}
		if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "incomplete") {
			response.BadRequest(c, errMsg)
			return
		}
		response.InternalError(c, "Restore failed: "+errMsg)
		return
	}

	response.Success(c, gin.H{
		"restored":     true,
		"need_restart": true,
		"message":      "Database restored successfully. Please restart the service.",
	})
}

func (h *DataManagementHandler) AbortRestoreUpload(c *gin.Context) {
	uploadID := strings.TrimSpace(c.Param("upload_id"))
	if uploadID == "" {
		response.BadRequest(c, "Missing upload_id")
		return
	}
	h.pgBackupService.AbortUpload(uploadID)
	response.Success(c, gin.H{"aborted": true})
}

func normalizeBackupIdempotencyKey(headerValue, bodyValue string) string {
	headerKey := strings.TrimSpace(headerValue)
	if headerKey != "" {
		return headerKey
	}
	return strings.TrimSpace(bodyValue)
}
