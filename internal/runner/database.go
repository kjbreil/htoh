package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// File represents a video file on disk with its metadata
type File struct {
	ID        uint      `gorm:"primaryKey"`
	Path      string    `gorm:"uniqueIndex;not null"` // absolute path
	Name      string    // filename
	Size      int64     // file size in bytes
	ModTime   time.Time // last modified time
	Status    string    // queued, processing, done, failed
	CreatedAt time.Time
	UpdatedAt time.Time
}

// MediaInfo stores ffprobe results, one-to-one with File
type MediaInfo struct {
	ID        uint   `gorm:"primaryKey"`
	FileID    uint   `gorm:"uniqueIndex;not null"` // foreign key
	File      *File  `gorm:"foreignKey:FileID"`

	// Container/Format
	Duration      float64 // in seconds
	FormatBitrate int64   // bits per second
	Container     string  // mkv, mp4, etc

	// Video Stream
	VideoCodec   string  // h264, hevc, etc
	VideoProfile string  // high, main, etc
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

// InitDB initializes the SQLite database with GORM
func InitDB(workDir string, debug bool) (*gorm.DB, error) {
	// Create workDir if it doesn't exist
	if err := os.MkdirAll(workDir, 0o755); err != nil {
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
	if err := db.AutoMigrate(&File{}, &MediaInfo{}); err != nil {
		return nil, fmt.Errorf("failed to migrate database: %w", err)
	}

	return db, nil
}

// GetFileByPath retrieves a file record by its path
func GetFileByPath(db *gorm.DB, path string) (*File, error) {
	var file File
	result := db.Where("path = ?", path).First(&file)
	if result.Error != nil {
		return nil, result.Error
	}
	return &file, nil
}

// CreateOrUpdateFile creates a new file record or updates an existing one
func CreateOrUpdateFile(db *gorm.DB, file *File) error {
	if file == nil {
		return fmt.Errorf("file cannot be nil")
	}

	// Try to find existing file by path
	var existing File
	result := db.Where("path = ?", file.Path).First(&existing)

	if result.Error == gorm.ErrRecordNotFound {
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

// CreateOrUpdateMediaInfo creates a new media info record or updates an existing one
func CreateOrUpdateMediaInfo(db *gorm.DB, info *MediaInfo) error {
	if info == nil {
		return fmt.Errorf("media info cannot be nil")
	}

	if info.FileID == 0 {
		return fmt.Errorf("media info must have a valid FileID")
	}

	// Try to find existing media info by FileID
	var existing MediaInfo
	result := db.Where("file_id = ?", info.FileID).First(&existing)

	if result.Error == gorm.ErrRecordNotFound {
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

// GetMediaInfoByFileID retrieves media info for a specific file
func GetMediaInfoByFileID(db *gorm.DB, fileID uint) (*MediaInfo, error) {
	var info MediaInfo
	result := db.Where("file_id = ?", fileID).First(&info)
	if result.Error != nil {
		return nil, result.Error
	}
	return &info, nil
}
