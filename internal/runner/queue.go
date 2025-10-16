package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gorm.io/gorm"
)

// LogBroadcaster is an interface for broadcasting log events.
type LogBroadcaster interface {
	BroadcastTaskLog(queueItemID, fileID uint, logLevel, message string, timestamp string)
	BroadcastProgressUpdate(
		queueItemID, fileID uint,
		fps float64,
		speed string,
		outTimeS float64,
		sizeBytes int64,
		device string,
	)
}

// QueueProcessor manages the transcoding queue.
type QueueProcessor struct {
	db              *gorm.DB
	cfg             Config
	mu              sync.RWMutex
	active          map[uint]bool // track active queue items
	broadcaster     LogBroadcaster
	htmlBroadcaster func(uint)
}

// NewQueueProcessor creates a new queue processor.
func NewQueueProcessor(db *gorm.DB, cfg Config) *QueueProcessor {
	return &QueueProcessor{
		db:     db,
		cfg:    cfg,
		active: make(map[uint]bool),
	}
}

// SetBroadcaster sets the log broadcaster for SSE events.
func (qp *QueueProcessor) SetBroadcaster(broadcaster LogBroadcaster) {
	qp.broadcaster = broadcaster
}

// SetHTMLBroadcaster sets the HTML broadcaster for queue item updates.
func (qp *QueueProcessor) SetHTMLBroadcaster(fn func(uint)) {
	qp.htmlBroadcaster = fn
}

// broadcastHTML broadcasts an HTML update for a queue item.
func (qp *QueueProcessor) broadcastHTML(queueItemID uint) {
	if qp.htmlBroadcaster != nil {
		qp.htmlBroadcaster(queueItemID)
	}
}

// logAndBroadcast creates a task log and broadcasts it via SSE.
func (qp *QueueProcessor) logAndBroadcast(queueItemID, fileID uint, logLevel, message string) {
	// Create log in database
	if err := CreateTaskLog(qp.db, queueItemID, fileID, logLevel, message); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create task log: %v\n", err)
	}

	// Broadcast via SSE if broadcaster is set
	if qp.broadcaster != nil {
		qp.broadcaster.BroadcastTaskLog(queueItemID, fileID, logLevel, message, time.Now().Format(time.RFC3339))
	}
}

// Start begins processing the queue.
func (qp *QueueProcessor) Start(ctx context.Context) error {
	// Create a worker pool
	jobs := make(chan *QueueItem, qp.cfg.Workers*2)
	var wg sync.WaitGroup

	// Start workers
	for i := range qp.cfg.Workers {
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

// feedQueue feeds items from the database into the job channel.
func (qp *QueueProcessor) feedQueue(jobs chan<- *QueueItem) {
	// Get next queued item
	item, err := GetNextQueueItem(qp.db)
	if errors.Is(err, gorm.ErrRecordNotFound) {
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

// worker processes jobs from the queue.
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

// processItem processes a single queue item.
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
	qp.broadcastHTML(item.ID)

	// Log: Task started
	qp.logAndBroadcast(item.ID, item.FileID, "info", fmt.Sprintf("Task started on worker %d", workerID))

	if !qp.cfg.Silent {
		fmt.Printf("[worker %d] Processing: %s\n", workerID, item.File.Name)
	}

	// Get media info for the file
	qp.logAndBroadcast(item.ID, item.FileID, "info", "Retrieving media information...")
	mediaInfo, err := GetMediaInfoByFileID(qp.db, item.FileID)
	if err != nil {
		errMsg := fmt.Sprintf("failed to get media info: %v", err)
		qp.logAndBroadcast(item.ID, item.FileID, "error", errMsg)
		var updateErr error
		if updateErr = UpdateQueueItemStatus(qp.db, item.ID, "failed", errMsg); updateErr != nil {
			fmt.Fprintf(os.Stderr, "[worker %d] Failed to update queue item status: %v\n", workerID, updateErr)
		}
		qp.broadcastHTML(item.ID)
		return
	}

	qp.logAndBroadcast(
		item.ID,
		item.FileID,
		"info",
		fmt.Sprintf("Media info retrieved: codec=%s, resolution=%dx%d, fps=%.2f, bitrate=%d",
			mediaInfo.VideoCodec, mediaInfo.Width, mediaInfo.Height, mediaInfo.FPS, mediaInfo.VideoBitrate),
	)

	// Check if file is H.264/AVC
	if mediaInfo.VideoCodec != CodecH264 && mediaInfo.VideoCodec != CodecAVC {
		errMsg := fmt.Sprintf("file is not H.264/AVC (codec: %s)", mediaInfo.VideoCodec)
		qp.logAndBroadcast(item.ID, item.FileID, "error", errMsg)
		var updateErr error
		if updateErr = UpdateQueueItemStatus(qp.db, item.ID, "failed", errMsg); updateErr != nil {
			fmt.Fprintf(os.Stderr, "[worker %d] Failed to update queue item status: %v\n", workerID, updateErr)
		}
		qp.broadcastHTML(item.ID)
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
	qp.logAndBroadcast(item.ID, item.FileID, "info", fmt.Sprintf("Quality settings determined: %v", quality))

	container := "mkv"
	faststart := false
	if qp.cfg.ForceMP4 {
		container = containerMP4
		faststart = true
	} else if qp.cfg.FaststartMP4 && mediaInfo.Container == containerMP4 {
		container = containerMP4
		faststart = true
	}

	qp.logAndBroadcast(
		item.ID,
		item.FileID,
		"info",
		fmt.Sprintf("Output format: %s (faststart: %v)", container, faststart),
	)

	// Build output path
	ext := filepath.Ext(item.File.Path)
	baseName := item.File.Name[:len(item.File.Name)-len(ext)]
	outTarget := filepath.Join(qp.cfg.WorkDir, fmt.Sprintf("%s.hevc.%s", baseName, container))

	qp.logAndBroadcast(item.ID, item.FileID, "info", fmt.Sprintf("Output path: %s", outTarget))

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

	// Start progress broadcasting goroutine
	progressCtx, cancelProgress := context.WithCancel(ctx)
	defer cancelProgress()

	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-progressCtx.Done():
				return
			case <-ticker.C:
				// Get current progress from prog
				if row := prog.GetProgress(workerID); row != nil && qp.broadcaster != nil {
					// Only broadcast if we have meaningful progress data
					if row.FPS > 0 || row.OutTimeS > 0 {
						qp.broadcaster.BroadcastProgressUpdate(
							item.ID,
							item.FileID,
							row.FPS,
							row.Speed,
							row.OutTimeS,
							row.SizeBytes,
							row.Device,
						)
					}
				}
			}
		}
	}()

	// Transcode
	qp.logAndBroadcast(item.ID, item.FileID, "info", "Starting transcoding...")
	var transcodeErr error
	if transcodeErr = transcode(ctx, qp.cfg, job, workerID, prog, func(msg string) {
		qp.logAndBroadcast(item.ID, item.FileID, "info", msg)
	}); transcodeErr != nil {
		errMsg := fmt.Sprintf("transcoding failed: %v", transcodeErr)
		qp.logAndBroadcast(item.ID, item.FileID, "error", errMsg)
		var updateErr error
		if updateErr = UpdateQueueItemStatus(qp.db, item.ID, "failed", errMsg); updateErr != nil {
			fmt.Fprintf(os.Stderr, "[worker %d] Failed to update queue item status: %v\n", workerID, updateErr)
		}
		qp.broadcastHTML(item.ID)

		// Update file status
		var fileErr error
		if fileErr = qp.db.Model(&File{}).Where("id = ?", item.FileID).Update("status", "failed").Error; fileErr != nil {
			fmt.Fprintf(os.Stderr, "[worker %d] Failed to update file status: %v\n", workerID, fileErr)
		}
		return
	}

	qp.logAndBroadcast(item.ID, item.FileID, "info", "Transcoding completed successfully")

	// Mark as done
	var doneErr error
	if doneErr = UpdateQueueItemStatus(qp.db, item.ID, "done", ""); doneErr != nil {
		fmt.Fprintf(os.Stderr, "[worker %d] Failed to mark queue item as done: %v\n", workerID, doneErr)
		return
	}
	qp.broadcastHTML(item.ID)

	// Update file status
	var statusErr error
	if statusErr = qp.db.Model(&File{}).Where("id = ?", item.FileID).Update("status", "done").Error; statusErr != nil {
		fmt.Fprintf(os.Stderr, "[worker %d] Failed to update file status: %v\n", workerID, statusErr)
	}

	qp.logAndBroadcast(item.ID, item.FileID, "info", "Task completed successfully")

	if !qp.cfg.Silent {
		fmt.Printf("[worker %d] Completed: %s\n", workerID, item.File.Name)
	}
}

// AddFileToQueue adds a file to the queue by path.
func (qp *QueueProcessor) AddFileToQueue(filePath string) error {
	// Get file from database
	file, err := GetFileByPath(qp.db, filePath)
	if errors.Is(err, gorm.ErrRecordNotFound) {
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

	if mediaInfo.VideoCodec != CodecH264 && mediaInfo.VideoCodec != CodecAVC {
		return fmt.Errorf("file is not H.264/AVC (codec: %s)", mediaInfo.VideoCodec)
	}

	// Add to queue
	return AddToQueue(qp.db, file.ID, 0)
}

// AddFolderToQueue adds all H.264/AVC files in a folder to the queue.
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

		if mediaInfo.VideoCodec != CodecH264 && mediaInfo.VideoCodec != CodecAVC {
			continue
		}

		// Add to queue
		var addErr error
		if addErr = AddToQueue(qp.db, file.ID, 0); addErr == nil {
			count++
		}
	}

	return count, nil
}
