package database

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Codec constants.
const (
	CodecH264 = "h264"
	CodecAVC  = "avc"
	CodecHEVC = "hevc"
)

// File represents a video file on disk with its metadata.
type File struct {
	ID               uint            `gorm:"primaryKey"                  json:"id"`
	Path             string          `gorm:"uniqueIndex;not null"        json:"path"`               // absolute path
	Name             string          `                                   json:"name"`               // filename
	Size             int64           `                                   json:"size"`               // file size in bytes
	ModTime          time.Time       `                                   json:"mod_time"`           // last modified time
	Status           string          `                                   json:"status"`             // queued, processing, done, failed
	QualityProfileID uint            `gorm:"default:0"                   json:"quality_profile_id"` // user's preferred quality profile (0 = use default)
	QualityProfile   *QualityProfile `gorm:"foreignKey:QualityProfileID" json:"quality_profile,omitempty"`
	CreatedAt        time.Time       `                                   json:"created_at"`
	UpdatedAt        time.Time       `                                   json:"updated_at"`
}

// MediaInfo stores ffprobe results, one-to-one with File.
type MediaInfo struct {
	ID     uint  `gorm:"primaryKey"`
	FileID uint  `gorm:"uniqueIndex;not null"` // foreign key
	File   *File `gorm:"foreignKey:FileID"`

	// Container/Format
	Duration      float64 // in seconds
	FormatBitrate int64   // bits per second
	Container     string  // mkv, mp4, etc

	// Video Stream
	VideoCodec   string // h264, hevc, etc
	VideoProfile string // high, main, etc
	Width        int
	Height       int
	CodedWidth   int
	CodedHeight  int
	FPS          float64
	AspectRatio  string // e.g. "16:9"
	VideoBitrate int64  // bits per second

	// Pixel/Color Info
	PixelFormat    string // yuv420p, etc
	BitDepth       int    // 8, 10, etc
	ChromaLocation string // left, center, etc
	ColorSpace     string // bt709, bt2020, etc
	ColorRange     string // tv, pc
	ColorPrimaries string // bt709, bt2020, etc

	CreatedAt time.Time
	UpdatedAt time.Time
}

// QualityProfile represents a transcoding quality profile.
type QualityProfile struct {
	ID                uint      `gorm:"primaryKey"           json:"id"`
	Name              string    `gorm:"uniqueIndex;not null" json:"name"`
	Description       string    `gorm:"type:text"            json:"description"`
	Mode              string    `gorm:"not null"             json:"mode"`               // "vbr", "cbr", or "cbr_percent"
	TargetBitrate     int64     `gorm:"default:0"            json:"target_bitrate"`     // for CBR mode in bits per second
	QualityLevel      int       `gorm:"default:0"            json:"quality_level"`      // for VBR mode (0-51 range)
	BitrateMultiplier float64   `gorm:"default:0"            json:"bitrate_multiplier"` // for VBR with bitrate hint (e.g., 0.5 = 50% of source)
	IsDefault         bool      `gorm:"default:false;index"  json:"is_default"`         // mark the default profile
	CreatedAt         time.Time `                            json:"created_at"`
	UpdatedAt         time.Time `                            json:"updated_at"`
}

// QueueItem represents a file in the transcoding queue.
type QueueItem struct {
	ID               uint            `gorm:"primaryKey"                  json:"id"`
	FileID           uint            `gorm:"index;not null"              json:"file_id"` // foreign key to File
	File             *File           `gorm:"foreignKey:FileID"           json:"file,omitempty"`
	QualityProfileID uint            `gorm:"index;not null"              json:"quality_profile_id"` // foreign key to QualityProfile
	QualityProfile   *QualityProfile `gorm:"foreignKey:QualityProfileID" json:"quality_profile,omitempty"`
	Status           string          `gorm:"index"                       json:"status"` // queued, processing, done, failed
	Priority         int             `gorm:"default:0"                   json:"priority"`
	ErrorMessage     string          `                                   json:"error_message,omitempty"` // error details if status is failed
	CreatedAt        time.Time       `                                   json:"created_at"`
	UpdatedAt        time.Time       `                                   json:"updated_at"`
}

// TaskLog represents a log entry for a transcoding task.
type TaskLog struct {
	ID          uint       `gorm:"primaryKey"             json:"id"`
	QueueItemID uint       `gorm:"index;not null"         json:"queue_item_id"` // foreign key to QueueItem
	QueueItem   *QueueItem `gorm:"foreignKey:QueueItemID" json:"queue_item,omitempty"`
	FileID      uint       `gorm:"index;not null"         json:"file_id"` // foreign key to File (for easier queries)
	File        *File      `gorm:"foreignKey:FileID"      json:"file,omitempty"`
	LogLevel    string     `gorm:"index"                  json:"log_level"` // info, warning, error
	Message     string     `gorm:"type:text"              json:"message"`   // log message
	CreatedAt   time.Time  `                              json:"created_at"`
}

// InitDB initializes the SQLite database with GORM.
func InitDB(workDir string, debug bool) (*gorm.DB, error) {
	// Create workDir if it doesn't exist
	if err := os.MkdirAll(workDir, 0o750); err != nil {
		return nil, fmt.Errorf("failed to create work directory: %w", err)
	}

	// Database file path
	dbPath := filepath.Join(workDir, ".opti_cache.db")

	// Configure GORM logger
	logLevel := logger.Silent
	if debug {
		logLevel = logger.Info
	}

	// Open database connection
	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{
		SkipDefaultTransaction:                   false,
		DefaultTransactionTimeout:                0,
		DefaultContextTimeout:                    0,
		NamingStrategy:                           nil,
		FullSaveAssociations:                     false,
		Logger:                                   logger.Default.LogMode(logLevel),
		NowFunc:                                  nil,
		DryRun:                                   false,
		PrepareStmt:                              false,
		PrepareStmtMaxSize:                       0,
		PrepareStmtTTL:                           0,
		DisableAutomaticPing:                     false,
		DisableForeignKeyConstraintWhenMigrating: false,
		IgnoreRelationshipsWhenMigrating:         false,
		DisableNestedTransaction:                 false,
		AllowGlobalUpdate:                        false,
		QueryFields:                              false,
		CreateBatchSize:                          0,
		TranslateError:                           false,
		PropagateUnscoped:                        false,
		ClauseBuilders:                           nil,
		ConnPool:                                 nil,
		Dialector:                                nil,
		Plugins:                                  nil,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Run migrations - use empty structs for GORM type inference
	if err := db.AutoMigrate(
		&File{
			ID:               0,
			Path:             "",
			Name:             "",
			Size:             0,
			ModTime:          time.Time{},
			Status:           "",
			QualityProfileID: 0,
			QualityProfile:   nil,
			CreatedAt:        time.Time{},
			UpdatedAt:        time.Time{},
		},
		&MediaInfo{},
		&QualityProfile{
			ID:                0,
			Name:              "",
			Description:       "",
			Mode:              "",
			TargetBitrate:     0,
			QualityLevel:      0,
			BitrateMultiplier: 0,
			IsDefault:         false,
			CreatedAt:         time.Time{},
			UpdatedAt:         time.Time{},
		},
		&QueueItem{
			ID:               0,
			FileID:           0,
			File:             nil,
			QualityProfileID: 0,
			QualityProfile:   nil,
			Status:           "",
			Priority:         0,
			ErrorMessage:     "",
			CreatedAt:        time.Time{},
			UpdatedAt:        time.Time{},
		},
		&TaskLog{},
	); err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	// Create default profile if no profiles exist
	var count int64
	// Use empty struct for GORM Model() type inference
	if err := db.Model(&QualityProfile{
		ID:                0,
		Name:              "",
		Description:       "",
		Mode:              "",
		TargetBitrate:     0,
		QualityLevel:      0,
		BitrateMultiplier: 0,
		IsDefault:         false,
		CreatedAt:         time.Time{},
		UpdatedAt:         time.Time{},
	}).Count(&count).Error; err != nil {
		return nil, fmt.Errorf("failed to count quality profiles: %w", err)
	}

	if count == 0 {
		if err := CreateDefaultProfile(db); err != nil {
			return nil, fmt.Errorf("failed to create default profile: %w", err)
		}
	}

	return db, nil
}

// GetFileByPath retrieves a file record by its path.
func GetFileByPath(db *gorm.DB, path string) (*File, error) {
	var file File
	result := db.Where("path = ?", path).First(&file)
	if result.Error != nil {
		return nil, result.Error
	}
	return &file, nil
}

// CreateOrUpdateFile creates a new file record or updates an existing one.
func CreateOrUpdateFile(db *gorm.DB, file *File) error {
	if file == nil {
		return errors.New("file cannot be nil")
	}

	// Try to find existing file by path
	var existing File
	result := db.Where("path = ?", file.Path).First(&existing)

	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		// Create new record
		if err := db.Create(file).Error; err != nil {
			return fmt.Errorf("failed to create file: %w", err)
		}
		return nil
	} else if result.Error != nil {
		return fmt.Errorf("failed to query file: %w", result.Error)
	}

	// Update existing record
	file.ID = existing.ID
	if err := db.Save(file).Error; err != nil {
		return fmt.Errorf("failed to update file: %w", err)
	}

	return nil
}

// CreateOrUpdateMediaInfo creates a new media info record or updates an existing one.
func CreateOrUpdateMediaInfo(db *gorm.DB, info *MediaInfo) error {
	if info == nil {
		return errors.New("media info cannot be nil")
	}

	if info.FileID == 0 {
		return errors.New("media info must have a valid FileID")
	}

	// Try to find existing media info by FileID
	var existing MediaInfo
	result := db.Where("file_id = ?", info.FileID).First(&existing)

	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		// Create new record
		if err := db.Create(info).Error; err != nil {
			return fmt.Errorf("failed to create media info: %w", err)
		}
		return nil
	} else if result.Error != nil {
		return fmt.Errorf("failed to query media info: %w", result.Error)
	}

	// Update existing record
	info.ID = existing.ID
	if err := db.Save(info).Error; err != nil {
		return fmt.Errorf("failed to update media info: %w", err)
	}

	return nil
}

// GetMediaInfoByFileID retrieves media info for a specific file.
func GetMediaInfoByFileID(db *gorm.DB, fileID uint) (*MediaInfo, error) {
	var info MediaInfo
	result := db.Where("file_id = ?", fileID).First(&info)
	if result.Error != nil {
		return nil, result.Error
	}
	return &info, nil
}

// IsEligibleForConversion determines if a file is eligible for transcoding
// based on its media info. This function provides backward compatibility
// and checks only for H.264/AVC codec (suitable for codec conversion).
// For profile-aware eligibility checks, use IsEligibleForConversionWithProfile.
func IsEligibleForConversion(mediaInfo *MediaInfo) bool {
	if mediaInfo == nil {
		return false
	}

	// Only H.264/AVC files are eligible for conversion to H.265/HEVC
	if mediaInfo.VideoCodec == CodecH264 || mediaInfo.VideoCodec == CodecAVC {
		return true
	}

	return false
}

// IsEligibleForConversionWithProfile determines if a file is eligible for transcoding
// based on its media info and the specified quality profile mode.
// This provides profile-aware eligibility checking:
// - For H.264/AVC: always eligible (all modes work - codec conversion or re-encoding)
// - For HEVC with "cbr_percent" mode: NOT eligible (cbr_percent only makes sense when changing codecs)
// - For HEVC with "vbr" or "cbr" mode: eligible (can re-encode HEVC to different quality)
// - For other codecs: not eligible.
func IsEligibleForConversionWithProfile(mediaInfo *MediaInfo, profile *QualityProfile) bool {
	if mediaInfo == nil || profile == nil {
		return false
	}

	// Normalize the mode string
	mode := GetQualityProfileMode(profile)

	// H.264/AVC is always eligible - all quality modes work for codec conversion
	if mediaInfo.VideoCodec == CodecH264 || mediaInfo.VideoCodec == CodecAVC {
		return true
	}

	// HEVC eligibility depends on the quality mode
	if mediaInfo.VideoCodec == CodecHEVC {
		// cbr_percent mode only makes sense when changing codecs (e.g., H.264 -> HEVC)
		// For HEVC files, cbr_percent has no meaningful interpretation since there's no codec change
		if mode == "cbr_percent" {
			return false
		}

		// vbr and cbr modes can re-encode HEVC files to different quality levels
		if mode == "vbr" || mode == "cbr" {
			return true
		}
	}

	// Other codecs are not supported
	return false
}

// GetQualityProfileMode returns the normalized quality profile mode string.
// Valid modes are: "vbr", "cbr", "cbr_percent".
// If the mode is invalid or empty, returns "vbr" as the default.
func GetQualityProfileMode(profile *QualityProfile) string {
	if profile == nil {
		return "vbr"
	}

	mode := profile.Mode
	if mode == "vbr" || mode == "cbr" || mode == "cbr_percent" {
		return mode
	}

	// Default to vbr for invalid modes
	return "vbr"
}

// CreateOrUpdateQualityProfile creates a new quality profile or updates an existing one.
func CreateOrUpdateQualityProfile(db *gorm.DB, profile *QualityProfile) error {
	if profile == nil {
		return errors.New("quality profile cannot be nil")
	}

	if profile.Name == "" {
		return errors.New("quality profile name cannot be empty")
	}

	// Try to find existing profile by name
	var existing QualityProfile
	result := db.Where("name = ?", profile.Name).First(&existing)

	if errors.Is(result.Error, gorm.ErrRecordNotFound) {
		// Create new record
		if err := db.Create(profile).Error; err != nil {
			return fmt.Errorf("failed to create quality profile: %w", err)
		}
		return nil
	} else if result.Error != nil {
		return fmt.Errorf("failed to query quality profile: %w", result.Error)
	}

	// Update existing record
	profile.ID = existing.ID
	if err := db.Save(profile).Error; err != nil {
		return fmt.Errorf("failed to update quality profile: %w", err)
	}

	return nil
}

// GetQualityProfileByID retrieves a quality profile by its ID.
func GetQualityProfileByID(db *gorm.DB, id uint) (*QualityProfile, error) {
	var profile QualityProfile
	result := db.Where("id = ?", id).First(&profile)
	if result.Error != nil {
		return nil, result.Error
	}
	return &profile, nil
}

// GetQualityProfileByName retrieves a quality profile by its name.
func GetQualityProfileByName(db *gorm.DB, name string) (*QualityProfile, error) {
	var profile QualityProfile
	result := db.Where("name = ?", name).First(&profile)
	if result.Error != nil {
		return nil, result.Error
	}
	return &profile, nil
}

// GetAllQualityProfiles retrieves all quality profiles.
func GetAllQualityProfiles(db *gorm.DB) ([]QualityProfile, error) {
	var profiles []QualityProfile
	result := db.Order("is_default DESC, name ASC").Find(&profiles)
	if result.Error != nil {
		return nil, result.Error
	}
	return profiles, nil
}

// GetDefaultQualityProfile retrieves the default quality profile.
func GetDefaultQualityProfile(db *gorm.DB) (*QualityProfile, error) {
	var profile QualityProfile
	result := db.Where("is_default = ?", true).First(&profile)
	if result.Error != nil {
		return nil, result.Error
	}
	return &profile, nil
}

// DeleteQualityProfile removes a quality profile by ID.
func DeleteQualityProfile(db *gorm.DB, id uint) error {
	// Check if profile is being used by any queue items
	var count int64
	// Use empty struct for GORM Model() type inference
	if err := db.Model(&QueueItem{
		ID:               0,
		FileID:           0,
		File:             nil,
		QualityProfileID: 0,
		QualityProfile:   nil,
		Status:           "",
		Priority:         0,
		ErrorMessage:     "",
		CreatedAt:        time.Time{},
		UpdatedAt:        time.Time{},
	}).Where("quality_profile_id = ?", id).Count(&count).Error; err != nil {
		return fmt.Errorf("failed to check queue items: %w", err)
	}

	if count > 0 {
		return fmt.Errorf("cannot delete quality profile: %d queue items are using this profile", count)
	}

	// Check if this is the default profile
	var profile QualityProfile
	result := db.Where("id = ?", id).First(&profile)
	if result.Error != nil {
		return fmt.Errorf("failed to find quality profile: %w", result.Error)
	}

	if profile.IsDefault {
		return errors.New("cannot delete the default quality profile")
	}

	// Use empty struct for GORM Delete() type inference
	if err := db.Delete(&QualityProfile{
		ID:                0,
		Name:              "",
		Description:       "",
		Mode:              "",
		TargetBitrate:     0,
		QualityLevel:      0,
		BitrateMultiplier: 0,
		IsDefault:         false,
		CreatedAt:         time.Time{},
		UpdatedAt:         time.Time{},
	}, id).Error; err != nil {
		return fmt.Errorf("failed to delete quality profile: %w", err)
	}
	return nil
}

// CreateDefaultProfile creates the default "Current Quality" profile that mimics existing auto-quality behavior.
func CreateDefaultProfile(db *gorm.DB) error {
	profile := &QualityProfile{
		ID:                0, // Will be auto-assigned by GORM
		Name:              "Current Quality",
		Description:       "Adaptive quality based on source bitrate (~50% reduction)",
		Mode:              "cbr_percent",
		TargetBitrate:     0,
		QualityLevel:      0,
		BitrateMultiplier: 0.5,
		IsDefault:         true,
		CreatedAt:         time.Time{}, // Will be auto-set by GORM
		UpdatedAt:         time.Time{}, // Will be auto-set by GORM
	}

	if err := db.Create(profile).Error; err != nil {
		return fmt.Errorf("failed to create default profile: %w", err)
	}

	return nil
}

// AddToQueue adds a file to the transcoding queue.
// Returns the queue item ID.
func AddToQueue(db *gorm.DB, fileID uint, priority int, qualityProfileID uint) (uint, error) {
	// Validate quality profile exists
	if qualityProfileID == 0 {
		return 0, errors.New("quality profile ID cannot be zero")
	}

	var profile QualityProfile
	result := db.Where("id = ?", qualityProfileID).First(&profile)
	if result.Error != nil {
		if errors.Is(result.Error, gorm.ErrRecordNotFound) {
			return 0, fmt.Errorf("quality profile with ID %d not found", qualityProfileID)
		}
		return 0, fmt.Errorf("failed to validate quality profile: %w", result.Error)
	}

	// Check if already in queue (queued or processing)
	var existing QueueItem
	result = db.Where("file_id = ? AND status IN ?", fileID, []string{"queued", "processing"}).First(&existing)

	if result.Error == nil {
		// Already in queue with active status
		return existing.ID, nil
	}

	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("failed to check queue: %w", result.Error)
	}

	// Check if there's any completed or failed record for this file
	var oldRecord QueueItem
	oldResult := db.Where("file_id = ?", fileID).First(&oldRecord)

	if oldResult.Error == nil {
		// Delete old record and its logs to ensure only one record per file
		if err := DeleteTaskLogsByQueueItem(db, oldRecord.ID); err != nil {
			return 0, fmt.Errorf("failed to delete task logs: %w", err)
		}
		if err := db.Delete(&oldRecord).Error; err != nil {
			return 0, fmt.Errorf("failed to delete old queue record: %w", err)
		}
	}

	// Add to queue
	queueItem := &QueueItem{
		ID:               0, // Will be auto-assigned by GORM
		FileID:           fileID,
		File:             nil, // Will be loaded via Preload when needed
		QualityProfileID: qualityProfileID,
		QualityProfile:   nil, // Will be loaded via Preload when needed
		Status:           "queued",
		Priority:         priority,
		ErrorMessage:     "",
		CreatedAt:        time.Time{}, // Will be auto-set by GORM
		UpdatedAt:        time.Time{}, // Will be auto-set by GORM
	}

	if err := db.Create(queueItem).Error; err != nil {
		return 0, fmt.Errorf("failed to add to queue: %w", err)
	}

	return queueItem.ID, nil
}

// GetNextQueueItem retrieves the next item to process from the queue.
func GetNextQueueItem(db *gorm.DB) (*QueueItem, error) {
	var item QueueItem
	result := db.Where("status = ?", "queued").
		Order("priority DESC, created_at ASC").
		Preload("File").
		Preload("QualityProfile").
		First(&item)

	if result.Error != nil {
		return nil, result.Error
	}

	return &item, nil
}

// UpdateQueueItemStatus updates the status of a queue item.
func UpdateQueueItemStatus(db *gorm.DB, id uint, status string, errorMsg string) error {
	updates := map[string]interface{}{
		"status": status,
	}

	if errorMsg != "" {
		updates["error_message"] = errorMsg
	}

	// Use empty struct for GORM Model() type inference
	if err := db.Model(&QueueItem{
		ID:               0,
		FileID:           0,
		File:             nil,
		QualityProfileID: 0,
		QualityProfile:   nil,
		Status:           "",
		Priority:         0,
		ErrorMessage:     "",
		CreatedAt:        time.Time{},
		UpdatedAt:        time.Time{},
	}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return fmt.Errorf("failed to update queue item status: %w", err)
	}

	return nil
}

// GetQueueItems retrieves all queue items with optional status filter.
func GetQueueItems(db *gorm.DB, status string) ([]QueueItem, error) {
	var items []QueueItem
	query := db.Preload("File").Preload("QualityProfile")

	if status != "" {
		query = query.Where("status = ?", status)
	}

	result := query.Order("priority DESC, created_at ASC").Find(&items)
	if result.Error != nil {
		return nil, result.Error
	}

	return items, nil
}

// DeleteQueueItem removes a queue item by ID.
func DeleteQueueItem(db *gorm.DB, id uint) error {
	// Use empty struct for GORM Delete() type inference
	if err := db.Delete(&QueueItem{
		ID:               0,
		FileID:           0,
		File:             nil,
		QualityProfileID: 0,
		QualityProfile:   nil,
		Status:           "",
		Priority:         0,
		ErrorMessage:     "",
		CreatedAt:        time.Time{},
		UpdatedAt:        time.Time{},
	}, id).Error; err != nil {
		return fmt.Errorf("failed to delete queue item: %w", err)
	}
	return nil
}

// CreateTaskLog creates a new task log entry.
func CreateTaskLog(db *gorm.DB, queueItemID, fileID uint, logLevel, message string) error {
	log := &TaskLog{
		ID:          0, // Will be auto-assigned by GORM
		QueueItemID: queueItemID,
		QueueItem:   nil, // Will be loaded via Preload when needed
		FileID:      fileID,
		File:        nil, // Will be loaded via Preload when needed
		LogLevel:    logLevel,
		Message:     message,
		CreatedAt:   time.Time{}, // Will be auto-set by GORM
	}

	if err := db.Create(log).Error; err != nil {
		return fmt.Errorf("failed to create task log: %w", err)
	}

	return nil
}

// GetTaskLogsByQueueItem retrieves all log entries for a specific queue item.
func GetTaskLogsByQueueItem(db *gorm.DB, queueItemID uint) ([]TaskLog, error) {
	var logs []TaskLog
	result := db.Where("queue_item_id = ?", queueItemID).Order("created_at ASC").Find(&logs)
	if result.Error != nil {
		return nil, result.Error
	}
	return logs, nil
}

// GetTaskLogsByFile retrieves all log entries for a specific file.
func GetTaskLogsByFile(db *gorm.DB, fileID uint) ([]TaskLog, error) {
	var logs []TaskLog
	result := db.Where("file_id = ?", fileID).Order("created_at ASC").Find(&logs)
	if result.Error != nil {
		return nil, result.Error
	}
	return logs, nil
}

// DeleteTaskLogsByQueueItem deletes all log entries for a queue item.
func DeleteTaskLogsByQueueItem(db *gorm.DB, queueItemID uint) error {
	// Use empty struct for GORM Delete() type inference
	if err := db.Where("queue_item_id = ?", queueItemID).Delete(&TaskLog{
		ID:          0,
		QueueItemID: 0,
		QueueItem:   nil,
		FileID:      0,
		File:        nil,
		LogLevel:    "",
		Message:     "",
		CreatedAt:   time.Time{},
	}).Error; err != nil {
		return fmt.Errorf("failed to delete task logs: %w", err)
	}
	return nil
}

// UpdateFileQualityProfile updates the quality profile preference for a file.
func UpdateFileQualityProfile(db *gorm.DB, fileID uint, qualityProfileID uint) error {
	// If qualityProfileID is not 0, validate that it exists
	if qualityProfileID != 0 {
		var profile QualityProfile
		result := db.Where("id = ?", qualityProfileID).First(&profile)
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("quality profile with ID %d not found", qualityProfileID)
			}
			return fmt.Errorf("failed to validate quality profile: %w", result.Error)
		}
	}

	// Update the file's quality profile ID - use empty struct for GORM Model() type inference
	if err := db.Model(&File{
		ID:               0,
		Path:             "",
		Name:             "",
		Size:             0,
		ModTime:          time.Time{},
		Status:           "",
		QualityProfileID: 0,
		QualityProfile:   nil,
		CreatedAt:        time.Time{},
		UpdatedAt:        time.Time{},
	}).Where("id = ?", fileID).Update("quality_profile_id", qualityProfileID).Error; err != nil {
		return fmt.Errorf("failed to update file quality profile: %w", err)
	}

	return nil
}

// UpdateFolderQualityProfile updates the quality profile for all files in a folder (recursively).
// This enables cascading profile changes down the directory tree.
func UpdateFolderQualityProfile(db *gorm.DB, folderPath string, qualityProfileID uint) error {
	// If qualityProfileID is not 0, validate that it exists
	if qualityProfileID != 0 {
		var profile QualityProfile
		result := db.Where("id = ?", qualityProfileID).First(&profile)
		if result.Error != nil {
			if errors.Is(result.Error, gorm.ErrRecordNotFound) {
				return fmt.Errorf("quality profile with ID %d not found", qualityProfileID)
			}
			return fmt.Errorf("failed to validate quality profile: %w", result.Error)
		}
	}

	// Update all files in the folder and subfolders
	// Use LIKE with % to match all files under this path
	// Use empty struct for GORM Model() type inference
	if err := db.Model(&File{
		ID:               0,
		Path:             "",
		Name:             "",
		Size:             0,
		ModTime:          time.Time{},
		Status:           "",
		QualityProfileID: 0,
		QualityProfile:   nil,
		CreatedAt:        time.Time{},
		UpdatedAt:        time.Time{},
	}).
		Where("path LIKE ?", folderPath+"%").
		Update("quality_profile_id", qualityProfileID).Error; err != nil {
		return fmt.Errorf("failed to update folder quality profiles: %w", err)
	}

	return nil
}

// GetFolderProfileAggregation determines the aggregated profile state for a folder.
// Returns the common profile ID if all files have the same profile, or indicates mixed state.
// Returns:
//   - (profileID, false, nil) if all files have the same profile
//   - (0, true, nil) if files have different profiles (mixed state)
//   - (0, false, nil) if no files found in the folder
func GetFolderProfileAggregation(db *gorm.DB, folderPath string) (uint, bool, error) {
	// Query all files under this folder path
	var files []File
	if err := db.Where("path LIKE ?", folderPath+"%").Find(&files).Error; err != nil {
		return 0, false, fmt.Errorf("failed to query files in folder: %w", err)
	}

	// No files found
	if len(files) == 0 {
		return 0, false, nil
	}

	// Check if all files have the same profile
	firstProfileID := files[0].QualityProfileID
	allSame := true
	for i := 1; i < len(files); i++ {
		if files[i].QualityProfileID != firstProfileID {
			allSame = false
			break
		}
	}

	if allSame {
		return firstProfileID, false, nil
	}

	// Mixed profiles
	return 0, true, nil
}

// FileFilters represents filter criteria for querying files.
type FileFilters struct {
	NeedsConvert bool   // Only H.264/AVC files eligible for conversion
	MinBitrate   int64  // Minimum bitrate in bits per second (0 = no filter)
	MaxBitrate   int64  // Maximum bitrate in bits per second (0 = no filter)
	MinSize      int64  // Minimum file size in bytes (0 = no filter)
	MaxSize      int64  // Maximum file size in bytes (0 = no filter)
	SourceDir    string // Filter by source directory path prefix (empty = no filter)
	Codec        string // Filter by video codec (empty = no filter)
}

// GetFilteredFiles retrieves files matching the given filter criteria.
// Returns files with preloaded MediaInfo and QualityProfile relationships.
func GetFilteredFiles(db *gorm.DB, filters FileFilters) ([]File, error) {
	query := db.Preload("QualityProfile").
		Joins("LEFT JOIN media_infos ON media_infos.file_id = files.id")

	// Apply NeedsConvert filter (H.264/AVC files only)
	if filters.NeedsConvert {
		query = query.Where("media_infos.video_codec IN ?", []string{CodecH264, CodecAVC})
	}

	// Apply codec filter
	if filters.Codec != "" {
		query = query.Where("media_infos.video_codec = ?", filters.Codec)
	}

	// Apply bitrate filters
	if filters.MinBitrate > 0 {
		query = query.Where("media_infos.video_bitrate >= ?", filters.MinBitrate)
	}
	if filters.MaxBitrate > 0 {
		query = query.Where("media_infos.video_bitrate <= ?", filters.MaxBitrate)
	}

	// Apply size filters
	if filters.MinSize > 0 {
		query = query.Where("files.size >= ?", filters.MinSize)
	}
	if filters.MaxSize > 0 {
		query = query.Where("files.size <= ?", filters.MaxSize)
	}

	// Apply source directory filter
	if filters.SourceDir != "" {
		query = query.Where("files.path LIKE ?", filters.SourceDir+"%")
	}

	// Execute query
	var files []File
	if err := query.Find(&files).Error; err != nil {
		return nil, fmt.Errorf("failed to query filtered files: %w", err)
	}

	return files, nil
}
