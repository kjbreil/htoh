package runner

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

// File represents a video file on disk with its metadata.
type File struct {
	ID        uint      `gorm:"primaryKey"           json:"id"`
	Path      string    `gorm:"uniqueIndex;not null" json:"path"`     // absolute path
	Name      string    `                            json:"name"`     // filename
	Size      int64     `                            json:"size"`     // file size in bytes
	ModTime   time.Time `                            json:"mod_time"` // last modified time
	Status    string    `                            json:"status"`   // queued, processing, done, failed
	CreatedAt time.Time `                            json:"created_at"`
	UpdatedAt time.Time `                            json:"updated_at"`
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

// QueueItem represents a file in the transcoding queue.
type QueueItem struct {
	ID           uint      `gorm:"primaryKey"        json:"id"`
	FileID       uint      `gorm:"index;not null"    json:"file_id"` // foreign key to File
	File         *File     `gorm:"foreignKey:FileID" json:"file,omitempty"`
	Status       string    `gorm:"index"             json:"status"` // queued, processing, done, failed
	Priority     int       `gorm:"default:0"         json:"priority"`
	QualityLevel int       `gorm:"default:0"         json:"quality_level"`           // for future use
	ErrorMessage string    `                         json:"error_message,omitempty"` // error details if status is failed
	CreatedAt    time.Time `                         json:"created_at"`
	UpdatedAt    time.Time `                         json:"updated_at"`
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
		Logger: logger.Default.LogMode(logLevel),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	// Run migrations
	if err := db.AutoMigrate(&File{}, &MediaInfo{}, &QueueItem{}, &TaskLog{}); err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
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

// AddToQueue adds a file to the transcoding queue.
func AddToQueue(db *gorm.DB, fileID uint, priority int) error {
	// Check if already in queue (queued or processing)
	var existing QueueItem
	result := db.Where("file_id = ? AND status IN ?", fileID, []string{"queued", "processing"}).First(&existing)

	if result.Error == nil {
		// Already in queue with active status
		return nil
	}

	if !errors.Is(result.Error, gorm.ErrRecordNotFound) {
		return fmt.Errorf("failed to check queue: %w", result.Error)
	}

	// Check if there's any completed or failed record for this file
	var oldRecord QueueItem
	oldResult := db.Where("file_id = ?", fileID).First(&oldRecord)

	if oldResult.Error == nil {
		// Delete old record and its logs to ensure only one record per file
		if err := DeleteTaskLogsByQueueItem(db, oldRecord.ID); err != nil {
			return fmt.Errorf("failed to delete task logs: %w", err)
		}
		if err := db.Delete(&oldRecord).Error; err != nil {
			return fmt.Errorf("failed to delete old queue record: %w", err)
		}
	}

	// Add to queue
	queueItem := &QueueItem{
		FileID:   fileID,
		Status:   "queued",
		Priority: priority,
	}

	if err := db.Create(queueItem).Error; err != nil {
		return fmt.Errorf("failed to add to queue: %w", err)
	}

	return nil
}

// GetNextQueueItem retrieves the next item to process from the queue.
func GetNextQueueItem(db *gorm.DB) (*QueueItem, error) {
	var item QueueItem
	result := db.Where("status = ?", "queued").
		Order("priority DESC, created_at ASC").
		Preload("File").
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

	if err := db.Model(&QueueItem{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return fmt.Errorf("failed to update queue item status: %w", err)
	}

	return nil
}

// GetQueueItems retrieves all queue items with optional status filter.
func GetQueueItems(db *gorm.DB, status string) ([]QueueItem, error) {
	var items []QueueItem
	query := db.Preload("File")

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
	if err := db.Delete(&QueueItem{}, id).Error; err != nil {
		return fmt.Errorf("failed to delete queue item: %w", err)
	}
	return nil
}

// CreateTaskLog creates a new task log entry.
func CreateTaskLog(db *gorm.DB, queueItemID, fileID uint, logLevel, message string) error {
	log := &TaskLog{
		QueueItemID: queueItemID,
		FileID:      fileID,
		LogLevel:    logLevel,
		Message:     message,
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
	if err := db.Where("queue_item_id = ?", queueItemID).Delete(&TaskLog{}).Error; err != nil {
		return fmt.Errorf("failed to delete task logs: %w", err)
	}
	return nil
}
