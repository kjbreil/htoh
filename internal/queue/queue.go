package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kjbreil/opti/internal/config"
	"github.com/kjbreil/opti/internal/database"
	"github.com/kjbreil/opti/internal/progress"
	"github.com/kjbreil/opti/internal/runner"
	"gorm.io/gorm"
)

const (
	// Queue polling and progress intervals.
	queuePollInterval     = 2 * time.Second        // how often to check for new queue items
	progressBroadcastRate = 500 * time.Millisecond // how often to broadcast progress updates

	// Worker pool sizing.
	jobChannelMultiplier = 2 // buffer size multiplier for job channel (workers * 2)
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
		elapsedSeconds float64,
		etaSeconds float64,
	)
	BroadcastQueueUpdated(queueID, fileID uint, status string)
}

// Processor manages the transcoding queue.
type Processor struct {
	db              *gorm.DB
	cfg             config.Config
	log             *slog.Logger
	mu              sync.RWMutex
	active          map[uint]context.CancelFunc // track active queue items and their cancel functions
	broadcaster     LogBroadcaster
	htmlBroadcaster func(uint)
}

// NewQueueProcessor creates a new queue processor.
func NewQueueProcessor(db *gorm.DB, cfg config.Config, log *slog.Logger) *Processor {
	return &Processor{
		db:              db,
		cfg:             cfg,
		log:             log,
		mu:              sync.RWMutex{},
		active:          make(map[uint]context.CancelFunc),
		broadcaster:     nil, // Set via SetBroadcaster
		htmlBroadcaster: nil, // Set via SetHTMLBroadcaster
	}
}

// SetBroadcaster sets the log broadcaster for SSE events.
func (qp *Processor) SetBroadcaster(broadcaster LogBroadcaster) {
	qp.broadcaster = broadcaster
}

// SetHTMLBroadcaster sets the HTML broadcaster for queue item updates.
func (qp *Processor) SetHTMLBroadcaster(fn func(uint)) {
	qp.htmlBroadcaster = fn
}

// CancelQueueItem cancels a running queue item by calling its context cancel function.
// Returns true if the item was actively processing and was cancelled, false otherwise.
func (qp *Processor) CancelQueueItem(itemID uint) bool {
	qp.mu.Lock()
	defer qp.mu.Unlock()

	cancelFunc, exists := qp.active[itemID]
	if !exists || cancelFunc == nil {
		return false
	}

	// Call the cancel function to stop the transcoding process
	cancelFunc()
	qp.log.Info("Cancelled queue item",
		slog.Uint64("queue_item_id", uint64(itemID)))

	return true
}

// broadcastHTML broadcasts an HTML update for a queue item.
func (qp *Processor) broadcastHTML(queueItemID uint) {
	if qp.htmlBroadcaster != nil {
		qp.htmlBroadcaster(queueItemID)
	}
}

// logAndBroadcast creates a task log and broadcasts it via SSE.
func (qp *Processor) logAndBroadcast(queueItemID, fileID uint, logLevel, message string) {
	// Create log in database
	if err := database.CreateTaskLog(qp.db, queueItemID, fileID, logLevel, message); err != nil {
		qp.log.Error("Failed to create task log",
			slog.Uint64("queue_item_id", uint64(queueItemID)),
			slog.Uint64("file_id", uint64(fileID)),
			slog.Any("error", err))
	}

	// Broadcast via SSE if broadcaster is set
	if qp.broadcaster != nil {
		qp.broadcaster.BroadcastTaskLog(queueItemID, fileID, logLevel, message, time.Now().Format(time.RFC3339))
	}
}

// broadcastProgressLoop broadcasts progress updates for a transcoding task.
func (qp *Processor) broadcastProgressLoop(
	ctx context.Context,
	queueItemID, fileID uint,
	_ int,
	prog *progress.Prog,
) {
	ticker := time.NewTicker(progressBroadcastRate)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			qp.broadcastProgressUpdate(queueItemID, fileID, prog)
		}
	}
}

// broadcastProgressUpdate broadcasts a single progress update.
func (qp *Processor) broadcastProgressUpdate(
	queueItemID, fileID uint,
	prog *progress.Prog,
) {
	if qp.broadcaster == nil {
		return
	}

	row := prog.GetProgress(0)
	if row == nil {
		return
	}

	// Only broadcast if we have meaningful progress data
	if row.FPS <= 0 && row.OutTimeS <= 0 {
		return
	}

	// Calculate elapsed time and ETA
	elapsed, eta := qp.calculateProgressTimes(row)

	qp.broadcaster.BroadcastProgressUpdate(
		queueItemID,
		fileID,
		row.FPS,
		row.Speed,
		row.OutTimeS,
		row.SizeBytes,
		row.Device,
		elapsed,
		eta,
	)
}

// calculateProgressTimes calculates elapsed time and ETA from a progress row.
func (qp *Processor) calculateProgressTimes(row *progress.Row) (float64, float64) {
	if row.StartTime.IsZero() {
		return 0, 0
	}

	elapsed := time.Since(row.StartTime).Seconds()

	// Calculate ETA only if we have meaningful progress
	var eta float64
	if row.OutTimeS > 0 && row.DurationS > row.OutTimeS {
		eta = elapsed * (row.DurationS - row.OutTimeS) / row.OutTimeS
	}

	return elapsed, eta
}

// Start begins processing the queue.
func (qp *Processor) Start(ctx context.Context) error {
	// Create a worker pool
	jobs := make(chan *database.QueueItem, qp.cfg.Workers*jobChannelMultiplier)
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
		ticker := time.NewTicker(queuePollInterval)
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
func (qp *Processor) feedQueue(jobs chan<- *database.QueueItem) {
	// Get next queued item
	item, err := database.GetNextQueueItem(qp.db)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// No items in queue
		return
	}
	if err != nil {
		if !qp.cfg.Silent {
			qp.log.Error("Error getting next queue item", slog.Any("error", err))
		}
		return
	}

	// Check if already active
	qp.mu.Lock()
	if qp.active[item.ID] != nil {
		qp.mu.Unlock()
		return
	}
	// Reserve this item with a placeholder (will be replaced with actual cancel func in processItem)
	qp.active[item.ID] = func() {}
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
func (qp *Processor) worker(ctx context.Context, workerID int, jobs <-chan *database.QueueItem) {
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
func (qp *Processor) processItem(ctx context.Context, workerID int, item *database.QueueItem) {
	// Create cancellable context for this queue item
	itemCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Store the cancel function so it can be called if item is deleted
	qp.mu.Lock()
	qp.active[item.ID] = cancel
	qp.mu.Unlock()

	defer func() {
		qp.mu.Lock()
		delete(qp.active, item.ID)
		qp.mu.Unlock()
	}()

	// Initialize processing
	if !qp.initializeProcessing(workerID, item) {
		return
	}

	// Get and validate media info
	mediaInfo, err := qp.getAndValidateMediaInfo(workerID, item)
	if err != nil {
		return
	}

	// Prepare transcoding job
	job, prog := qp.prepareTranscodeJob(0, item, mediaInfo)

	// Execute transcoding with cancellable context
	if !qp.executeTranscode(itemCtx, workerID, item, job, prog) {
		return
	}

	// Post-processing
	qp.postProcessTranscode(itemCtx, workerID, item)
}

// initializeProcessing sets up the queue item for processing.
func (qp *Processor) initializeProcessing(workerID int, item *database.QueueItem) bool {
	if err := database.UpdateQueueItemStatus(qp.db, item.ID, "processing", ""); err != nil {
		qp.log.Error("Failed to update queue item status",
			slog.Int("worker_id", workerID),
			slog.Uint64("queue_item_id", uint64(item.ID)),
			slog.Any("error", err))
		return false
	}
	qp.broadcastHTML(item.ID)
	qp.logAndBroadcast(item.ID, item.FileID, "info", fmt.Sprintf("Task started on worker %d", workerID))

	if !qp.cfg.Silent {
		qp.log.Info("Processing file",
			slog.Int("worker_id", workerID),
			slog.Uint64("queue_item_id", uint64(item.ID)),
			slog.String("file", item.File.Name))
	}
	return true
}

// getAndValidateMediaInfo retrieves and validates media information for the queue item.
func (qp *Processor) getAndValidateMediaInfo(
	workerID int,
	item *database.QueueItem,
) (*database.MediaInfo, error) {
	qp.logAndBroadcast(item.ID, item.FileID, "info", "Retrieving media information...")
	var err error
	mediaInfo, err := database.GetMediaInfoByFileID(qp.db, item.FileID)
	if err != nil {
		qp.handleProcessingError(workerID, item, fmt.Sprintf("failed to get media info: %v", err))
		return nil, fmt.Errorf("failed to get media info: %w", err)
	}

	qp.logAndBroadcast(
		item.ID,
		item.FileID,
		"info",
		fmt.Sprintf("Media info retrieved: codec=%s, resolution=%dx%d, fps=%.2f, bitrate=%d",
			mediaInfo.VideoCodec, mediaInfo.Width, mediaInfo.Height, mediaInfo.FPS, mediaInfo.VideoBitrate),
	)

	// Validate quality profile
	if item.QualityProfile == nil {
		qp.handleProcessingError(workerID, item, "quality profile not found for queue item")
		return nil, errors.New("quality profile not found")
	}

	// Check eligibility
	if !database.IsEligibleForConversionWithProfile(mediaInfo, item.QualityProfile) {
		errMsg := fmt.Sprintf("file is not eligible for conversion with profile %s (codec: %s, mode: %s)",
			item.QualityProfile.Name, mediaInfo.VideoCodec, item.QualityProfile.Mode)
		qp.handleProcessingError(workerID, item, errMsg)
		return nil, errors.New(errMsg)
	}

	return mediaInfo, nil
}

// prepareTranscodeJob prepares the transcoding job configuration.
func (qp *Processor) prepareTranscodeJob(
	workerID int,
	item *database.QueueItem,
	mediaInfo *database.MediaInfo,
) (runner.Job, *progress.Prog) {
	// Build ProbeInfo for quality estimation
	probeInfo := &runner.ProbeInfo{
		VideoCodec: mediaInfo.VideoCodec,
		Width:      mediaInfo.Width,
		Height:     mediaInfo.Height,
		FPS:        mediaInfo.FPS,
		BitRate:    mediaInfo.VideoBitrate,
	}

	// Determine quality from profile
	quality := runner.DeriveQualityFromProfile(item.QualityProfile, probeInfo)
	qp.logAndBroadcast(
		item.ID,
		item.FileID,
		"info",
		fmt.Sprintf("Using quality profile: %s (%s: CRF=%d, ICQ=%d, CQ=%d, target=%d)",
			item.QualityProfile.Name,
			quality.Mode,
			quality.CRF,
			quality.ICQ,
			quality.CQ,
			quality.TargetBitrate),
	)

	// Determine output container
	container, faststart := qp.determineOutputContainer(mediaInfo)
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
	job := runner.Job{
		Src:       item.File.Path,
		Rel:       "", // Not used in queue mode
		Probe:     probeInfo,
		OutTarget: outTarget,
		Container: container,
		Faststart: faststart,
		Quality:   quality,
	}

	// Create progress tracker
	prog := progress.NewProg()
	prog.Update(workerID, func(r *progress.Row) {
		r.StartTime = time.Now()
		r.DurationS = mediaInfo.Duration
	})

	return job, prog
}

// determineOutputContainer determines the output container format and faststart flag.
func (qp *Processor) determineOutputContainer(mediaInfo *database.MediaInfo) (string, bool) {
	container := "mkv"
	faststart := false

	if qp.cfg.ForceMP4 {
		container = runner.ContainerMP4
		faststart = true
	} else if qp.cfg.FaststartMP4 && mediaInfo.Container == runner.ContainerMP4 {
		container = runner.ContainerMP4
		faststart = true
	}

	return container, faststart
}

// executeTranscode performs the actual transcoding operation.
func (qp *Processor) executeTranscode(
	ctx context.Context,
	workerID int,
	item *database.QueueItem,
	job runner.Job,
	prog *progress.Prog,
) bool {
	// Start progress broadcasting goroutine
	progressCtx, cancelProgress := context.WithCancel(ctx)
	defer cancelProgress()

	go qp.broadcastProgressLoop(progressCtx, item.ID, item.FileID, 0, prog)

	// Transcode
	qp.logAndBroadcast(item.ID, item.FileID, "info", "Starting transcoding...")
	var transcodeErr error
	if transcodeErr = runner.Transcode(ctx, qp.cfg, job, workerID, prog, func(msg string) {
		qp.logAndBroadcast(item.ID, item.FileID, "info", msg)
	}, qp.log); transcodeErr != nil {
		// Check if the error is due to context cancellation
		if errors.Is(transcodeErr, context.Canceled) {
			qp.logAndBroadcast(item.ID, item.FileID, "info", "Transcoding cancelled by user")
			qp.log.Info("Queue item cancelled",
				slog.Int("worker_id", workerID),
				slog.Uint64("queue_item_id", uint64(item.ID)))

			// Update queue item status to cancelled
			var statusErr error
			if statusErr = database.UpdateQueueItemStatus(qp.db, item.ID, "cancelled", "Cancelled by user"); statusErr != nil {
				qp.log.Error("Failed to update queue item status to cancelled",
					slog.Int("worker_id", workerID),
					slog.Uint64("queue_item_id", uint64(item.ID)),
					slog.Any("error", statusErr))
			}

			// Broadcast queue updated event
			if qp.broadcaster != nil {
				qp.broadcaster.BroadcastQueueUpdated(item.ID, item.FileID, "cancelled")
			}
			qp.broadcastHTML(item.ID)

			return false
		}

		// Regular error (not cancellation)
		errMsg := fmt.Sprintf("transcoding failed: %v", transcodeErr)
		qp.logAndBroadcast(item.ID, item.FileID, "error", errMsg)
		qp.handleProcessingError(workerID, item, errMsg)

		// Update file status
		var fileErr error
		if fileErr = qp.db.Model(&database.File{
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
		}).Where("id = ?", item.FileID).Update("status", "failed").Error; fileErr != nil {
			qp.log.Error("Failed to update file status",
				slog.Int("worker_id", workerID),
				slog.Uint64("file_id", uint64(item.FileID)),
				slog.Any("error", fileErr))
		}
		return false
	}

	qp.logAndBroadcast(item.ID, item.FileID, "info", "Transcoding completed successfully")
	return true
}

// postProcessTranscode handles post-transcoding analysis and status updates.
func (qp *Processor) postProcessTranscode(ctx context.Context, workerID int, item *database.QueueItem) {
	// Re-analyze the converted file
	qp.logAndBroadcast(item.ID, item.FileID, "info", "Re-analyzing converted file...")

	// Update file stats
	qp.updateFileStats(workerID, item)

	// Re-probe and update media info
	qp.updateMediaInfo(ctx, workerID, item)

	// Mark as done
	var doneErr error
	if doneErr = database.UpdateQueueItemStatus(qp.db, item.ID, "done", ""); doneErr != nil {
		qp.log.Error("Failed to mark queue item as done",
			slog.Int("worker_id", workerID),
			slog.Uint64("queue_item_id", uint64(item.ID)),
			slog.Any("error", doneErr))
		return
	}
	qp.broadcastHTML(item.ID)

	// Broadcast queue updated event
	if qp.broadcaster != nil {
		qp.broadcaster.BroadcastQueueUpdated(item.ID, item.FileID, "done")
	}

	// Update file status
	var statusErr error
	if statusErr = qp.db.Model(&database.File{
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
	}).Where("id = ?", item.FileID).Update("status", "done").Error; statusErr != nil {
		qp.log.Error("Failed to update file status",
			slog.Int("worker_id", workerID),
			slog.Uint64("file_id", uint64(item.FileID)),
			slog.Any("error", statusErr))
	}

	qp.logAndBroadcast(item.ID, item.FileID, "info", "Task completed successfully")

	if !qp.cfg.Silent {
		qp.log.Info("Task completed",
			slog.Int("worker_id", workerID),
			slog.Uint64("queue_item_id", uint64(item.ID)),
			slog.String("file", item.File.Name))
	}
}

// updateFileStats updates the file size and modification time after transcoding.
func (qp *Processor) updateFileStats(_ int, item *database.QueueItem) {
	var fileInfo os.FileInfo
	var statErr error
	fileInfo, statErr = os.Stat(item.File.Path)
	if statErr != nil {
		qp.logAndBroadcast(item.ID, item.FileID, "warning",
			fmt.Sprintf("Failed to stat converted file: %v", statErr))
		return
	}

	var updateErr = qp.db.Model(&database.File{
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
		Where("id = ?", item.FileID).
		Updates(map[string]interface{}{
			"size":     fileInfo.Size(),
			"mod_time": fileInfo.ModTime(),
		}).Error
	if updateErr != nil {
		qp.logAndBroadcast(item.ID, item.FileID, "warning",
			fmt.Sprintf("Failed to update file stats: %v", updateErr))
	}
}

// updateMediaInfo re-probes the file and updates media information in the database.
func (qp *Processor) updateMediaInfo(ctx context.Context, _ int, item *database.QueueItem) {
	var probeResult *runner.ProbeInfoDetailed
	var probeErr error
	probeResult, probeErr = runner.Probe(ctx, qp.cfg.FFprobePath, item.File.Path)
	if probeErr != nil {
		qp.logAndBroadcast(item.ID, item.FileID, "warning",
			fmt.Sprintf("Failed to re-probe converted file: %v", probeErr))
		return
	}

	updatedMediaInfo := &database.MediaInfo{
		ID:             0, // Will be set by CreateOrUpdateMediaInfo
		FileID:         item.FileID,
		File:           nil, // Will be loaded via Preload when needed
		Duration:       probeResult.Duration,
		FormatBitrate:  probeResult.FormatBitrate,
		Container:      probeResult.Container,
		VideoCodec:     probeResult.VideoCodec,
		VideoProfile:   probeResult.VideoProfile,
		Width:          probeResult.Width,
		Height:         probeResult.Height,
		CodedWidth:     probeResult.CodedWidth,
		CodedHeight:    probeResult.CodedHeight,
		FPS:            probeResult.FPS,
		AspectRatio:    probeResult.AspectRatio,
		VideoBitrate:   probeResult.VideoBitrate,
		PixelFormat:    probeResult.PixelFormat,
		BitDepth:       probeResult.BitDepth,
		ChromaLocation: probeResult.ChromaLocation,
		ColorSpace:     probeResult.ColorSpace,
		ColorRange:     probeResult.ColorRange,
		ColorPrimaries: probeResult.ColorPrimaries,
		CreatedAt:      time.Time{}, // Will be auto-set by GORM
		UpdatedAt:      time.Time{}, // Will be auto-set by GORM
	}

	var saveErr error
	if saveErr = database.CreateOrUpdateMediaInfo(qp.db, updatedMediaInfo); saveErr != nil {
		qp.logAndBroadcast(item.ID, item.FileID, "warning",
			fmt.Sprintf("Failed to update media info: %v", saveErr))
	} else {
		qp.logAndBroadcast(item.ID, item.FileID, "info",
			fmt.Sprintf("Re-analysis complete: codec=%s, resolution=%dx%d, bitrate=%d",
				updatedMediaInfo.VideoCodec, updatedMediaInfo.Width, updatedMediaInfo.Height,
				updatedMediaInfo.VideoBitrate))
	}
}

// handleProcessingError handles errors during item processing.
func (qp *Processor) handleProcessingError(workerID int, item *database.QueueItem, errMsg string) {
	qp.logAndBroadcast(item.ID, item.FileID, "error", errMsg)
	var updateErr error
	if updateErr = database.UpdateQueueItemStatus(qp.db, item.ID, "failed", errMsg); updateErr != nil {
		qp.log.Error("Failed to update queue item status",
			slog.Int("worker_id", workerID),
			slog.Uint64("queue_item_id", uint64(item.ID)),
			slog.Any("error", updateErr))
	}
	qp.broadcastHTML(item.ID)
}

// AddFileToQueue adds a file to the queue by path.
// Returns the queue item ID.
// If qualityProfileID is 0, uses the default profile.
// If qualityProfileID is non-zero, validates that the profile exists.
func (qp *Processor) AddFileToQueue(filePath string, qualityProfileID uint) (uint, error) {
	// Get file from database
	file, err := database.GetFileByPath(qp.db, filePath)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, fmt.Errorf("file not found in database: %s", filePath)
	}
	if err != nil {
		return 0, fmt.Errorf("failed to get file: %w", err)
	}

	// Get media info
	var mediaInfo *database.MediaInfo
	mediaInfo, err = database.GetMediaInfoByFileID(qp.db, file.ID)
	if err != nil {
		return 0, fmt.Errorf("failed to get media info: %w", err)
	}

	// Determine quality profile to use
	var profile *database.QualityProfile
	var profileID uint
	if qualityProfileID == 0 {
		// Get default quality profile
		profile, err = database.GetDefaultQualityProfile(qp.db)
		if err != nil {
			return 0, fmt.Errorf("failed to get default quality profile: %w", err)
		}
		profileID = profile.ID
	} else {
		// Validate that the specified profile exists
		profile, err = database.GetQualityProfileByID(qp.db, qualityProfileID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, fmt.Errorf("quality profile with ID %d not found", qualityProfileID)
		}
		if err != nil {
			return 0, fmt.Errorf("failed to get quality profile: %w", err)
		}
		profileID = profile.ID
	}

	// Check if file is eligible for conversion with this profile
	if !database.IsEligibleForConversionWithProfile(mediaInfo, profile) {
		return 0, fmt.Errorf("file is not eligible for conversion with profile %s (codec: %s, mode: %s)",
			profile.Name, mediaInfo.VideoCodec, profile.Mode)
	}

	// Add to queue
	queueItemID, err := database.AddToQueue(qp.db, file.ID, 0, profileID)
	if err != nil {
		return 0, fmt.Errorf("failed to add file to queue: %w", err)
	}
	return queueItemID, nil
}

// AddFolderToQueue adds all eligible files in a folder to the queue.
// If qualityProfileID is 0, uses the default profile.
// If qualityProfileID is non-zero, validates that the profile exists.
func (qp *Processor) AddFolderToQueue(folderPath string, qualityProfileID uint) (int, error) {
	count := 0

	// Determine quality profile to use
	var profile *database.QualityProfile
	var profileID uint
	var err error
	if qualityProfileID == 0 {
		// Get default quality profile
		profile, err = database.GetDefaultQualityProfile(qp.db)
		if err != nil {
			return 0, fmt.Errorf("failed to get default quality profile: %w", err)
		}
		profileID = profile.ID
	} else {
		// Validate that the specified profile exists
		profile, err = database.GetQualityProfileByID(qp.db, qualityProfileID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, fmt.Errorf("quality profile with ID %d not found", qualityProfileID)
		}
		if err != nil {
			return 0, fmt.Errorf("failed to get quality profile: %w", err)
		}
		profileID = profile.ID
	}

	// Get all files in the folder from database
	var files []database.File
	if err = qp.db.Where("path LIKE ?", folderPath+"%").Find(&files).Error; err != nil {
		return 0, fmt.Errorf("failed to query files: %w", err)
	}

	for _, file := range files {
		// Get media info
		var mediaInfo *database.MediaInfo
		mediaInfo, err = database.GetMediaInfoByFileID(qp.db, file.ID)
		if err != nil {
			continue
		}

		// Check if file is eligible for conversion with this profile
		if !database.IsEligibleForConversionWithProfile(mediaInfo, profile) {
			continue
		}

		// Add to queue
		var addErr error
		if _, addErr = database.AddToQueue(qp.db, file.ID, 0, profileID); addErr == nil {
			count++
		}
	}

	return count, nil
}
