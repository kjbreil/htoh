package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gorm.io/gorm"
)

// QueueProcessor manages the transcoding queue
type QueueProcessor struct {
	db     *gorm.DB
	cfg    Config
	mu     sync.RWMutex
	active map[uint]bool // track active queue items
}

// NewQueueProcessor creates a new queue processor
func NewQueueProcessor(db *gorm.DB, cfg Config) *QueueProcessor {
	return &QueueProcessor{
		db:     db,
		cfg:    cfg,
		active: make(map[uint]bool),
	}
}

// Start begins processing the queue
func (qp *QueueProcessor) Start(ctx context.Context) error {
	// Create a worker pool
	jobs := make(chan *QueueItem, qp.cfg.Workers*2)
	var wg sync.WaitGroup

	// Start workers
	for i := 0; i < qp.cfg.Workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			qp.worker(ctx, workerID, jobs)
		}(i)
	}

	// Queue feeder - continuously check for new work
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				close(jobs)
				return
			case <-ticker.C:
				qp.feedQueue(jobs)
			}
		}
	}()

	// Wait for all workers to finish
	wg.Wait()
	return nil
}

// feedQueue feeds items from the database into the job channel
func (qp *QueueProcessor) feedQueue(jobs chan<- *QueueItem) {
	// Get next queued item
	item, err := GetNextQueueItem(qp.db)
	if err == gorm.ErrRecordNotFound {
		// No items in queue
		return
	}
	if err != nil {
		if !qp.cfg.Silent {
			fmt.Fprintf(os.Stderr, "Error getting next queue item: %v\n", err)
		}
		return
	}

	// Check if already active
	qp.mu.Lock()
	if qp.active[item.ID] {
		qp.mu.Unlock()
		return
	}
	qp.active[item.ID] = true
	qp.mu.Unlock()

	// Try to send to jobs channel (non-blocking)
	select {
	case jobs <- item:
		// Successfully queued
	default:
		// Channel full, mark as not active
		qp.mu.Lock()
		delete(qp.active, item.ID)
		qp.mu.Unlock()
	}
}

// worker processes jobs from the queue
func (qp *QueueProcessor) worker(ctx context.Context, workerID int, jobs <-chan *QueueItem) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-jobs:
			if !ok {
				return
			}
			qp.processItem(ctx, workerID, item)
		}
	}
}

// processItem processes a single queue item
func (qp *QueueProcessor) processItem(ctx context.Context, workerID int, item *QueueItem) {
	defer func() {
		qp.mu.Lock()
		delete(qp.active, item.ID)
		qp.mu.Unlock()
	}()

	// Update status to processing
	if err := UpdateQueueItemStatus(qp.db, item.ID, "processing", ""); err != nil {
		fmt.Fprintf(os.Stderr, "[worker %d] Failed to update queue item status: %v\n", workerID, err)
		return
	}

	if !qp.cfg.Silent {
		fmt.Printf("[worker %d] Processing: %s\n", workerID, item.File.Name)
	}

	// Get media info for the file
	mediaInfo, err := GetMediaInfoByFileID(qp.db, item.FileID)
	if err != nil {
		errMsg := fmt.Sprintf("failed to get media info: %v", err)
		UpdateQueueItemStatus(qp.db, item.ID, "failed", errMsg)
		return
	}

	// Check if file is H.264/AVC
	if mediaInfo.VideoCodec != "h264" && mediaInfo.VideoCodec != "avc" {
		errMsg := fmt.Sprintf("file is not H.264/AVC (codec: %s)", mediaInfo.VideoCodec)
		UpdateQueueItemStatus(qp.db, item.ID, "failed", errMsg)
		return
	}

	// Build ProbeInfo for quality estimation
	probeInfo := &ProbeInfo{
		VideoCodec: mediaInfo.VideoCodec,
		Width:      mediaInfo.Width,
		Height:     mediaInfo.Height,
		FPS:        mediaInfo.FPS,
		BitRate:    mediaInfo.VideoBitrate,
	}

	// Determine quality and output container
	quality := deriveQualityChoice(probeInfo, qp.cfg.FastMode)

	container := "mkv"
	faststart := false
	if qp.cfg.ForceMP4 {
		container = "mp4"
		faststart = true
	} else if qp.cfg.FaststartMP4 && mediaInfo.Container == "mp4" {
		container = "mp4"
		faststart = true
	}

	// Build output path
	ext := filepath.Ext(item.File.Path)
	baseName := item.File.Name[:len(item.File.Name)-len(ext)]
	outTarget := filepath.Join(qp.cfg.WorkDir, fmt.Sprintf("%s.hevc.%s", baseName, container))

	// Create job
	job := job{
		Src:       item.File.Path,
		Probe:     probeInfo,
		OutTarget: outTarget,
		Quality:   quality,
		Container: container,
		Faststart: faststart,
	}

	// Create a simple progress tracker (no live dashboard for queue mode)
	prog := NewProg()

	// Transcode
	if err := transcode(ctx, qp.cfg, job, workerID, prog); err != nil {
		errMsg := fmt.Sprintf("transcoding failed: %v", err)
		UpdateQueueItemStatus(qp.db, item.ID, "failed", errMsg)

		// Update file status
		if err := qp.db.Model(&File{}).Where("id = ?", item.FileID).Update("status", "failed").Error; err != nil {
			fmt.Fprintf(os.Stderr, "[worker %d] Failed to update file status: %v\n", workerID, err)
		}
		return
	}

	// Mark as done
	if err := UpdateQueueItemStatus(qp.db, item.ID, "done", ""); err != nil {
		fmt.Fprintf(os.Stderr, "[worker %d] Failed to mark queue item as done: %v\n", workerID, err)
		return
	}

	// Update file status
	if err := qp.db.Model(&File{}).Where("id = ?", item.FileID).Update("status", "done").Error; err != nil {
		fmt.Fprintf(os.Stderr, "[worker %d] Failed to update file status: %v\n", workerID, err)
	}

	if !qp.cfg.Silent {
		fmt.Printf("[worker %d] Completed: %s\n", workerID, item.File.Name)
	}
}

// AddFileToQueue adds a file to the queue by path
func (qp *QueueProcessor) AddFileToQueue(filePath string) error {
	// Get file from database
	file, err := GetFileByPath(qp.db, filePath)
	if err == gorm.ErrRecordNotFound {
		return fmt.Errorf("file not found in database: %s", filePath)
	}
	if err != nil {
		return fmt.Errorf("failed to get file: %w", err)
	}

	// Check if file is H.264/AVC
	mediaInfo, err := GetMediaInfoByFileID(qp.db, file.ID)
	if err != nil {
		return fmt.Errorf("failed to get media info: %w", err)
	}

	if mediaInfo.VideoCodec != "h264" && mediaInfo.VideoCodec != "avc" {
		return fmt.Errorf("file is not H.264/AVC (codec: %s)", mediaInfo.VideoCodec)
	}

	// Add to queue
	return AddToQueue(qp.db, file.ID, 0)
}

// AddFolderToQueue adds all H.264/AVC files in a folder to the queue
func (qp *QueueProcessor) AddFolderToQueue(folderPath string) (int, error) {
	count := 0

	// Get all files in the folder from database
	var files []File
	if err := qp.db.Where("path LIKE ?", folderPath+"%").Find(&files).Error; err != nil {
		return 0, fmt.Errorf("failed to query files: %w", err)
	}

	for _, file := range files {
		// Check if file is H.264/AVC
		mediaInfo, err := GetMediaInfoByFileID(qp.db, file.ID)
		if err != nil {
			continue
		}

		if mediaInfo.VideoCodec != "h264" && mediaInfo.VideoCodec != "avc" {
			continue
		}

		// Add to queue
		if err := AddToQueue(qp.db, file.ID, 0); err == nil {
			count++
		}
	}

	return count, nil
}
