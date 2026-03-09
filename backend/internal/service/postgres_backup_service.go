package service

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
)

const (
	ChunkSize             = 16 * 1024 * 1024 // 16 MB
	uploadSessionTimeout  = 30 * time.Minute
	uploadCleanupInterval = 5 * time.Minute
)

// PostgresBackupService provides PostgreSQL database backup and restore via pg_dump/pg_restore.
type PostgresBackupService struct {
	dbCfg    config.DatabaseConfig
	mu       sync.Mutex
	sessions map[string]*uploadSession
}

type uploadSession struct {
	ID         string
	Dir        string
	Filename   string
	TotalSize  int64
	ChunkCount int
	Received   map[int]bool
	CreatedAt  time.Time
}

// NewPostgresBackupService creates a new PostgresBackupService from the application config.
func NewPostgresBackupService(cfg *config.Config) *PostgresBackupService {
	svc := &PostgresBackupService{
		dbCfg:    cfg.Database,
		sessions: make(map[string]*uploadSession),
	}
	go svc.cleanupLoop()
	return svc
}

// PostgresInfo returns sanitized database connection info (no password).
type PostgresInfo struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	DBName  string `json:"dbname"`
	User    string `json:"user"`
	SSLMode string `json:"sslmode"`
}

// GetInfo returns the current database connection info (password redacted).
func (s *PostgresBackupService) GetInfo() PostgresInfo {
	return PostgresInfo{
		Host:    s.dbCfg.Host,
		Port:    s.dbCfg.Port,
		DBName:  s.dbCfg.DBName,
		User:    s.dbCfg.User,
		SSLMode: s.dbCfg.SSLMode,
	}
}

// ExportFilename returns a suggested filename for the backup.
func (s *PostgresBackupService) ExportFilename() string {
	ts := time.Now().UTC().Format("20060102-150405")
	return fmt.Sprintf("sub2api-postgres-%s.dump", ts)
}

// Export runs pg_dump in custom format and writes the output to w.
func (s *PostgresBackupService) Export(ctx context.Context, w io.Writer) error {
	args := []string{
		"-Fc",
		"--no-owner",
		"--no-privileges",
		"-h", s.dbCfg.Host,
		"-p", fmt.Sprintf("%d", s.dbCfg.Port),
		"-U", s.dbCfg.User,
		"-d", s.dbCfg.DBName,
	}

	cmd := exec.CommandContext(ctx, "pg_dump", args...)
	cmd.Env = s.pgEnv()
	cmd.Stdout = w

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("pg_dump failed: %s", errMsg)
		}
		return fmt.Errorf("pg_dump failed: %w", err)
	}
	return nil
}

// Restore runs pg_restore from the reader (custom format dump).
func (s *PostgresBackupService) Restore(ctx context.Context, r io.Reader) error {
	args := []string{
		"--clean",
		"--if-exists",
		"--no-owner",
		"--no-privileges",
		"-h", s.dbCfg.Host,
		"-p", fmt.Sprintf("%d", s.dbCfg.Port),
		"-U", s.dbCfg.User,
		"-d", s.dbCfg.DBName,
	}

	cmd := exec.CommandContext(ctx, "pg_restore", args...)
	cmd.Env = s.pgEnv()
	cmd.Stdin = r

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		// pg_restore may return exit code 1 for warnings (e.g. "role does not exist")
		// but still successfully restore data. Only treat exit code > 1 as fatal.
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		if errMsg != "" {
			return fmt.Errorf("pg_restore failed: %s", errMsg)
		}
		return fmt.Errorf("pg_restore failed: %w", err)
	}
	return nil
}

// CheckTools verifies that pg_dump and pg_restore are available on PATH.
func (s *PostgresBackupService) CheckTools() error {
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s not found in PATH: %w", tool, err)
		}
	}
	return nil
}

func (s *PostgresBackupService) pgEnv() []string {
	env := []string{
		fmt.Sprintf("PGPASSWORD=%s", s.dbCfg.Password),
	}
	if s.dbCfg.SSLMode != "" {
		env = append(env, fmt.Sprintf("PGSSLMODE=%s", s.dbCfg.SSLMode))
	}
	return env
}

// InitUpload creates a new chunked upload session and returns the upload ID.
func (s *PostgresBackupService) InitUpload(filename string, totalSize int64) (string, int, error) {
	if totalSize <= 0 {
		return "", 0, fmt.Errorf("total_size must be > 0")
	}

	id, err := generateUploadID()
	if err != nil {
		return "", 0, fmt.Errorf("failed to generate upload id: %w", err)
	}

	dir, err := os.MkdirTemp("", "pg-restore-"+id+"-")
	if err != nil {
		return "", 0, fmt.Errorf("failed to create temp dir: %w", err)
	}

	chunkCount := int((totalSize + ChunkSize - 1) / ChunkSize)

	sess := &uploadSession{
		ID:         id,
		Dir:        dir,
		Filename:   filename,
		TotalSize:  totalSize,
		ChunkCount: chunkCount,
		Received:   make(map[int]bool),
		CreatedAt:  time.Now(),
	}

	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()

	return id, chunkCount, nil
}

// SaveChunk writes a chunk to disk for the given upload session.
func (s *PostgresBackupService) SaveChunk(uploadID string, index int, data io.Reader) (bool, error) {
	s.mu.Lock()
	sess, ok := s.sessions[uploadID]
	s.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("upload session not found: %s", uploadID)
	}
	if index < 0 || index >= sess.ChunkCount {
		return false, fmt.Errorf("chunk index %d out of range [0, %d)", index, sess.ChunkCount)
	}

	chunkPath := filepath.Join(sess.Dir, fmt.Sprintf("chunk_%05d", index))
	f, err := os.Create(chunkPath)
	if err != nil {
		return false, fmt.Errorf("failed to create chunk file: %w", err)
	}
	if _, err := io.Copy(f, data); err != nil {
		f.Close()
		return false, fmt.Errorf("failed to write chunk: %w", err)
	}
	f.Close()

	s.mu.Lock()
	sess.Received[index] = true
	allDone := len(sess.Received) == sess.ChunkCount
	s.mu.Unlock()

	return allDone, nil
}

// CompleteUpload merges chunks and runs pg_restore.
func (s *PostgresBackupService) CompleteUpload(ctx context.Context, uploadID, confirm string) error {
	s.mu.Lock()
	sess, ok := s.sessions[uploadID]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("upload session not found: %s", uploadID)
	}
	if len(sess.Received) != sess.ChunkCount {
		return fmt.Errorf("incomplete upload: received %d/%d chunks", len(sess.Received), sess.ChunkCount)
	}

	dbName := s.dbCfg.DBName
	expected := "RESTORE " + dbName
	if confirm != expected {
		return fmt.Errorf("confirmation text must be exactly: %s", expected)
	}

	// Merge chunks into a single file
	merged := filepath.Join(sess.Dir, "merged.dump")
	mf, err := os.Create(merged)
	if err != nil {
		return fmt.Errorf("failed to create merged file: %w", err)
	}

	indices := make([]int, 0, sess.ChunkCount)
	for i := range sess.Received {
		indices = append(indices, i)
	}
	sort.Ints(indices)

	for _, idx := range indices {
		chunkPath := filepath.Join(sess.Dir, fmt.Sprintf("chunk_%05d", idx))
		cf, err := os.Open(chunkPath)
		if err != nil {
			mf.Close()
			return fmt.Errorf("failed to open chunk %d: %w", idx, err)
		}
		if _, err := io.Copy(mf, cf); err != nil {
			cf.Close()
			mf.Close()
			return fmt.Errorf("failed to copy chunk %d: %w", idx, err)
		}
		cf.Close()
	}
	mf.Close()

	// Run pg_restore with file path (not stdin) for large files
	if err := s.RestoreFromFile(ctx, merged); err != nil {
		return err
	}

	// Cleanup
	s.mu.Lock()
	delete(s.sessions, uploadID)
	s.mu.Unlock()
	os.RemoveAll(sess.Dir)

	return nil
}

// RestoreFromFile runs pg_restore using a file path (better for large dumps).
func (s *PostgresBackupService) RestoreFromFile(ctx context.Context, filePath string) error {
	args := []string{
		"--clean",
		"--if-exists",
		"--no-owner",
		"--no-privileges",
		"-h", s.dbCfg.Host,
		"-p", fmt.Sprintf("%d", s.dbCfg.Port),
		"-U", s.dbCfg.User,
		"-d", s.dbCfg.DBName,
		filePath,
	}

	cmd := exec.CommandContext(ctx, "pg_restore", args...)
	cmd.Env = s.pgEnv()

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		if errMsg != "" {
			return fmt.Errorf("pg_restore failed: %s", errMsg)
		}
		return fmt.Errorf("pg_restore failed: %w", err)
	}
	return nil
}

// AbortUpload cancels an upload session and cleans up.
func (s *PostgresBackupService) AbortUpload(uploadID string) {
	s.mu.Lock()
	sess, ok := s.sessions[uploadID]
	if ok {
		delete(s.sessions, uploadID)
	}
	s.mu.Unlock()
	if ok {
		os.RemoveAll(sess.Dir)
	}
}

// GetUploadProgress returns current progress for an upload session.
func (s *PostgresBackupService) GetUploadProgress(uploadID string) (received, total int, found bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[uploadID]
	if !ok {
		return 0, 0, false
	}
	return len(sess.Received), sess.ChunkCount, true
}

func (s *PostgresBackupService) cleanupLoop() {
	ticker := time.NewTicker(uploadCleanupInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for id, sess := range s.sessions {
			if now.Sub(sess.CreatedAt) > uploadSessionTimeout {
				delete(s.sessions, id)
				go os.RemoveAll(sess.Dir)
			}
		}
		s.mu.Unlock()
	}
}

func generateUploadID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ChunkSizeBytes returns the chunk size for client reference.
func (s *PostgresBackupService) ChunkSizeBytes() int {
	return ChunkSize
}
