package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kjbreil/opti/internal/config"
	"github.com/kjbreil/opti/internal/database"
	"github.com/kjbreil/opti/internal/queue"
	"github.com/kjbreil/opti/internal/scanner"
	"gorm.io/gorm"
)

const (
	// HTTP server timeouts.
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second

	// Event channel buffer size.
	eventChannelBuffer = 10

	// Bitrate conversion factors.
	bitsPerMegabit = 1000000 // 1 Mbps = 1,000,000 bps
	bytesPerKiB    = 1024    // 1 KiB = 1,024 bytes

	// Size conversion factors.
	bytesPerGiB = 1024 * 1024 * 1024 // 1 GiB = 1,073,741,824 bytes
)

// Server represents the web server.
type Server struct {
	db          *gorm.DB
	cfg         config.Config
	broadcaster *EventBroadcaster
	queueProc   *queue.Processor
	templates   *template.Template
	log         *slog.Logger
}

// NewServer creates a new web server.
func NewServer(db *gorm.DB, cfg config.Config, queueProc *queue.Processor, log *slog.Logger) (*Server, error) {
	broadcaster := NewEventBroadcaster()

	// Set broadcaster on queue processor for real-time log events
	queueProc.SetBroadcaster(broadcaster)

	// Parse templates
	tmpl := template.New("").Funcs(template.FuncMap{
		"formatSize":  formatSize,
		"formatCodec": formatCodec,
	})

	// Load root templates
	tmpl, err := tmpl.ParseGlob(filepath.Join("internal", "web", "templates", "*.html"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}

	// Load partials (if they exist)
	partialsPath := filepath.Join("internal", "web", "templates", "partials", "*.html")
	if matches, _ := filepath.Glob(partialsPath); len(matches) > 0 {
		tmpl, err = tmpl.ParseGlob(partialsPath)
		if err != nil {
			return nil, fmt.Errorf("failed to parse partial templates: %w", err)
		}
	}

	s := &Server{
		db:          db,
		cfg:         cfg,
		broadcaster: broadcaster,
		queueProc:   queueProc,
		templates:   tmpl,
		log:         log,
	}

	// Set HTML broadcaster for queue item status updates
	queueProc.SetHTMLBroadcaster(func(queueItemID uint) {
		var item database.QueueItem
		var loadErr error
		if loadErr = db.Preload("File").First(&item, queueItemID).Error; loadErr != nil {
			log.Error("Failed to load queue item for HTML render",
				slog.Uint64("queue_item_id", uint64(queueItemID)),
				slog.Any("error", loadErr))
			return
		}

		var html string
		var renderErr error
		html, renderErr = s.renderQueueItemHTML(&item)
		if renderErr != nil {
			log.Error("Failed to render queue item HTML",
				slog.Uint64("queue_item_id", uint64(queueItemID)),
				slog.Any("error", renderErr))
			return
		}

		broadcaster.BroadcastQueueItemHTML(queueItemID, html)
	})

	return s, nil
}

// Start starts the HTTP server.
func (s *Server) Start(ctx context.Context, port int) error {
	mux := http.NewServeMux()

	// Routes
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/tree", s.handleTree)
	mux.HandleFunc("/api/convert", s.handleConvert)
	mux.HandleFunc("/api/scan", s.handleScan)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/queue", s.handleQueue)
	mux.HandleFunc("/api/queue/item", s.handleQueueItem)
	mux.HandleFunc("/api/queue/delete", s.handleQueueDelete)
	mux.HandleFunc("/api/queue/delete-bulk", s.handleQueueDeleteBulk)
	mux.HandleFunc("/api/queue/logs", s.handleQueueLogs)

	// Quality profile routes
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/api/profiles/get", s.handleProfileGet)
	mux.HandleFunc("/api/profiles/create", s.handleProfileCreate)
	mux.HandleFunc("/api/profiles/update", s.handleProfileUpdate)
	mux.HandleFunc("/api/profiles/delete", s.handleProfileDelete)
	// RESTful routes with ID in path (must come after exact matches)
	mux.HandleFunc("/api/profiles/", s.handleProfileByID)

	// File quality profile route
	mux.HandleFunc("/api/files/set-quality-profile", s.handleSetFileQualityProfile)
	// Folder quality profile route
	mux.HandleFunc("/api/folders/set-quality-profile", s.handleSetFolderQualityProfile)

	// Filtered files routes
	mux.HandleFunc("/api/files/filtered", s.handleFilesFiltered)
	mux.HandleFunc("/api/files/bulk-convert", s.handleBulkConvert)

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			s.log.Error("Server shutdown error", slog.Any("error", err))
		}
	}()

	s.log.Info("Web server starting", slog.String("address", addr))
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// handleIndex serves the main page.
func (s *Server) handleIndex(w http.ResponseWriter, _ *http.Request) {
	if err := s.templates.ExecuteTemplate(w, "index.html", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// TreeNode represents a node in the directory tree.
type TreeNode struct {
	Name             string       `json:"name"`
	Path             string       `json:"path"`
	IsDir            bool         `json:"is_dir"`
	FileID           uint         `json:"file_id,omitempty"`
	Codec            string       `json:"codec,omitempty"`
	Size             int64        `json:"size,omitempty"`
	Status           string       `json:"status,omitempty"`
	CanConvert       bool         `json:"can_convert,omitempty"`
	QualityProfileID uint         `json:"quality_profile_id,omitempty"` // user's preferred quality profile (0 = use default)
	ProfileIsMixed   bool         `json:"profile_is_mixed,omitempty"`   // true if folder contains files with different profiles
	Children         []TreeNode   `json:"children,omitempty"`
	Stats            *FolderStats `json:"stats,omitempty"`
}

// FolderStats represents aggregate statistics for a folder.
type FolderStats struct {
	TotalFiles int            `json:"total_files"`
	TotalSize  int64          `json:"total_size"`
	CodecCount map[string]int `json:"codec_count"`
	H264Count  int            `json:"h264_count"`
}

// handleTree returns the directory tree structure.
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")

	if path != "" {
		s.handleSingleTreePath(w, path)
		return
	}

	// No specific path - return tree(s) for all source directories
	if len(s.cfg.SourceDirs) == 0 {
		http.Error(w, "No source directories configured", http.StatusInternalServerError)
		return
	}

	if len(s.cfg.SourceDirs) == 1 {
		s.handleSingleTreePath(w, s.cfg.SourceDirs[0])
		return
	}

	// Multiple source directories - return array of TreeNode
	s.handleMultipleTreePaths(w)
}

// handleSingleTreePath returns a single tree for a specific path.
func (s *Server) handleSingleTreePath(w http.ResponseWriter, path string) {
	tree, err := s.buildTree(path)
	if err != nil {
		s.log.Error("Failed to build tree",
			slog.String("endpoint", "/api/tree"),
			slog.String("path", path),
			slog.Any("error", err))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Validate UTF-8 in tree structure
	if invalidPaths := s.validateTreeUTF8(tree); len(invalidPaths) > 0 {
		s.log.Error("Tree contains invalid UTF-8 characters",
			slog.String("endpoint", "/api/tree"),
			slog.String("path", path),
			slog.Int("invalid_path_count", len(invalidPaths)),
			slog.Any("invalid_paths", invalidPaths))
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(tree); encodeErr != nil {
		s.log.Error("Failed to encode JSON response",
			slog.String("endpoint", "/api/tree"),
			slog.String("path", path),
			slog.Any("error", encodeErr))
	}
}

// handleMultipleTreePaths returns trees for all source directories.
func (s *Server) handleMultipleTreePaths(w http.ResponseWriter) {
	trees := make([]TreeNode, 0, len(s.cfg.SourceDirs))
	for _, sourceDir := range s.cfg.SourceDirs {
		tree, err := s.buildTree(sourceDir)
		if err != nil {
			// Log error but continue with other directories
			s.log.Error("Failed to build tree for source directory",
				slog.String("endpoint", "/api/tree"),
				slog.String("source_dir", sourceDir),
				slog.Any("error", err))
			continue
		}

		// Validate UTF-8 in tree structure
		if invalidPaths := s.validateTreeUTF8(tree); len(invalidPaths) > 0 {
			s.log.Error("Tree contains invalid UTF-8 characters",
				slog.String("endpoint", "/api/tree"),
				slog.String("source_dir", sourceDir),
				slog.Int("invalid_path_count", len(invalidPaths)),
				slog.Any("invalid_paths", invalidPaths))
		}

		trees = append(trees, *tree)
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(trees); encodeErr != nil {
		s.log.Error("Failed to encode JSON response",
			slog.String("endpoint", "/api/tree"),
			slog.Int("tree_count", len(trees)),
			slog.Any("error", encodeErr))
	}
}

// buildTree builds a directory tree structure.
func (s *Server) buildTree(rootPath string) (*TreeNode, error) {
	s.log.Debug("Building tree",
		slog.String("root_path", rootPath))

	// Get default quality profile for convertibility checks
	defaultProfile, err := database.GetDefaultQualityProfile(s.db)
	if err != nil {
		// If no default profile exists, log it but continue (all files will be non-convertible)
		s.log.Warn("No default quality profile found",
			slog.String("root_path", rootPath),
			slog.Any("error", err))
	}

	// Get file info
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat path %s: %w", rootPath, err)
	}

	root := &TreeNode{
		Name:             filepath.Base(rootPath),
		Path:             rootPath,
		IsDir:            info.IsDir(),
		FileID:           0,
		Codec:            "",
		Size:             0,
		Status:           "",
		CanConvert:       false,
		QualityProfileID: 0,
		ProfileIsMixed:   false,
		Children:         nil,
		Stats:            nil,
	}

	if !info.IsDir() {
		// Single file
		var file *database.File
		file, err = database.GetFileByPath(s.db, rootPath)
		if err == nil {
			root.FileID = file.ID
			root.Size = file.Size
			root.Status = file.Status
			root.QualityProfileID = file.QualityProfileID

			var mediaInfo *database.MediaInfo
			mediaInfo, err = database.GetMediaInfoByFileID(s.db, file.ID)
			if err == nil {
				root.Codec = mediaInfo.VideoCodec
				// Check if file is convertible with default profile
				if defaultProfile != nil {
					root.CanConvert = database.IsEligibleForConversionWithProfile(mediaInfo, defaultProfile)
				}
			} else {
				s.log.Warn("Failed to get media info for file",
					slog.String("file_path", rootPath),
					slog.Uint64("file_id", uint64(file.ID)),
					slog.Any("error", err))
			}
		} else {
			s.log.Warn("Failed to get file from database",
				slog.String("file_path", rootPath),
				slog.Any("error", err))
		}
		return root, nil
	}

	// Directory - get all files from database
	var files []database.File
	var dbErr error
	if dbErr = s.db.Where("path LIKE ?", rootPath+"%").Find(&files).Error; dbErr != nil {
		s.log.Error("Failed to query files from database",
			slog.String("root_path", rootPath),
			slog.Any("error", dbErr))
		return nil, dbErr
	}

	s.log.Debug("Queried files from database",
		slog.String("root_path", rootPath),
		slog.Int("file_count", len(files)))

	// Build file map
	fileMap := make(map[string]*database.File)
	for i := range files {
		fileMap[files[i].Path] = &files[i]
	}

	// Walk directory structure
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", rootPath, err)
	}

	s.log.Debug("Read directory entries",
		slog.String("root_path", rootPath),
		slog.Int("entry_count", len(entries)))

	// Process entries
	for _, entry := range entries {
		childPath := filepath.Join(rootPath, entry.Name())

		if entry.IsDir() {
			// Recursive for directories
			var childNode *TreeNode
			childNode, err = s.buildTree(childPath)
			if err != nil {
				s.log.Warn("Failed to build tree for subdirectory, skipping",
					slog.String("parent_path", rootPath),
					slog.String("child_path", childPath),
					slog.Any("error", err))
				continue
			}
			root.Children = append(root.Children, *childNode)
		} else {
			// File
			ext := strings.ToLower(filepath.Ext(entry.Name()))
			if ext != ".mkv" && ext != ".mp4" && ext != ".mov" && ext != ".m4v" {
				continue
			}

			childNode := &TreeNode{
				Name:             entry.Name(),
				Path:             childPath,
				IsDir:            false,
				FileID:           0,
				Codec:            "",
				Size:             0,
				Status:           "",
				CanConvert:       false,
				QualityProfileID: 0,
				ProfileIsMixed:   false,
				Children:         nil,
				Stats:            nil,
			}

			if file, ok := fileMap[childPath]; ok {
				childNode.FileID = file.ID
				childNode.Size = file.Size
				childNode.Status = file.Status
				childNode.QualityProfileID = file.QualityProfileID

				var mediaInfo *database.MediaInfo
				mediaInfo, err = database.GetMediaInfoByFileID(s.db, file.ID)
				if err == nil {
					childNode.Codec = mediaInfo.VideoCodec
					// Check if file is convertible with default profile
					if defaultProfile != nil {
						childNode.CanConvert = database.IsEligibleForConversionWithProfile(mediaInfo, defaultProfile)
					}
				} else {
					s.log.Warn("Failed to get media info for child file",
						slog.String("parent_path", rootPath),
						slog.String("child_path", childPath),
						slog.Uint64("file_id", uint64(file.ID)),
						slog.Any("error", err))
				}
			} else {
				s.log.Debug("File not found in database",
					slog.String("parent_path", rootPath),
					slog.String("child_path", childPath))
			}

			root.Children = append(root.Children, *childNode)
		}
	}

	// Sort children: directories first, then files
	sort.Slice(root.Children, func(i, j int) bool {
		if root.Children[i].IsDir != root.Children[j].IsDir {
			return root.Children[i].IsDir
		}
		return root.Children[i].Name < root.Children[j].Name
	})

	// Calculate folder stats
	root.Stats = s.calculateFolderStats(&root.Children)

	// Calculate folder profile aggregation (only for directories)
	if root.IsDir {
		var profileID uint
		var isMixed bool
		var aggErr error
		profileID, isMixed, aggErr = database.GetFolderProfileAggregation(s.db, rootPath)
		if aggErr == nil {
			root.QualityProfileID = profileID
			root.ProfileIsMixed = isMixed
		} else {
			s.log.Warn("Failed to get folder profile aggregation",
				slog.String("root_path", rootPath),
				slog.Any("error", aggErr))
		}
	}

	s.log.Debug("Completed building tree",
		slog.String("root_path", rootPath),
		slog.Int("child_count", len(root.Children)))

	return root, nil
}

// calculateFolderStats calculates aggregate statistics for a folder.
func (s *Server) calculateFolderStats(children *[]TreeNode) *FolderStats {
	stats := &FolderStats{
		TotalFiles: 0,
		TotalSize:  0,
		CodecCount: make(map[string]int),
		H264Count:  0,
	}

	for _, child := range *children {
		if child.IsDir {
			if child.Stats != nil {
				stats.TotalFiles += child.Stats.TotalFiles
				stats.TotalSize += child.Stats.TotalSize
				stats.H264Count += child.Stats.H264Count
				for codec, count := range child.Stats.CodecCount {
					stats.CodecCount[codec] += count
				}
			}
		} else {
			stats.TotalFiles++
			stats.TotalSize += child.Size
			if child.Codec != "" {
				stats.CodecCount[child.Codec]++
				if strings.ToLower(child.Codec) == "h264" || strings.ToLower(child.Codec) == "avc" {
					stats.H264Count++
				}
			}
		}
	}

	return stats
}

// handleConvert handles conversion requests.
func (s *Server) handleConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.FormValue("path")
	isDir := r.FormValue("is_dir") == "true"
	qualityProfileIDStr := r.FormValue("quality_profile_id")

	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	// Parse quality profile ID (0 = use default)
	var qualityProfileID uint64
	if qualityProfileIDStr != "" {
		if _, err := fmt.Sscanf(qualityProfileIDStr, "%d", &qualityProfileID); err != nil {
			http.Error(w, "invalid quality_profile_id", http.StatusBadRequest)
			return
		}
		// Check for overflow when converting uint64 to uint
		if qualityProfileID > uint64(^uint(0)) {
			http.Error(w, "quality_profile_id too large", http.StatusBadRequest)
			return
		}
	}

	var count int
	var err error
	var queueItemID uint

	if isDir {
		count, err = s.queueProc.AddFolderToQueue(path, uint(qualityProfileID))
	} else {
		queueItemID, err = s.queueProc.AddFileToQueue(path, uint(qualityProfileID))
		if err == nil {
			count = 1
		}
	}

	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": err.Error(),
			"count":   0,
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Broadcast event - for single files use the actual queue item ID
	if !isDir && queueItemID > 0 {
		s.broadcaster.BroadcastQueueAdded(queueItemID, path)
	} else {
		s.broadcaster.BroadcastQueueAdded(0, path)
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   count,
		"message": fmt.Sprintf("Added %d file(s) to queue", count),
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleScan triggers a manual scan of all source directories.
func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Import scanner package
	var fileCount int
	var err error
	fileCount, err = scanner.ScanOnce(r.Context(), s.cfg, s.db, s.log)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Broadcast scan complete event
	s.broadcaster.BroadcastScanComplete(fileCount)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":    true,
		"file_count": fileCount,
		"message":    fmt.Sprintf("Scan complete: %d files found", fileCount),
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleEvents handles SSE connections.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Create client channel
	client := make(chan Event, eventChannelBuffer)
	s.broadcaster.Register(client)
	defer s.broadcaster.Unregister(client)

	// Send initial connection event
	fmt.Fprintf(w, "event: connected\ndata: {\"message\":\"Connected to event stream\"}\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Stream events
	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-client:
			fmt.Fprint(w, FormatSSE(event))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

// handleQueue returns the current queue status.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	items, err := database.GetQueueItems(s.db, status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(items); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// formatSize formats a byte size as human-readable string.
func formatSize(size int64) string {
	if size < bytesPerKiB {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(bytesPerKiB), 0
	for n := size / bytesPerKiB; n >= bytesPerKiB; n /= bytesPerKiB {
		div *= bytesPerKiB
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(size)/float64(div), "KMGTPE"[exp])
}

// formatCodec formats a codec name for display.
func formatCodec(codec string) string {
	codec = strings.ToUpper(codec)
	switch codec {
	case "H264", "AVC":
		return "H.264"
	case "HEVC", "H265":
		return "H.265"
	default:
		return codec
	}
}

// validateTreeUTF8 recursively validates UTF-8 encoding in a tree node and its children.
// Returns a slice of paths that contain invalid UTF-8 characters.
func (s *Server) validateTreeUTF8(node *TreeNode) []string {
	var invalidPaths []string

	// Check current node's string fields
	if !utf8.ValidString(node.Name) {
		invalidPaths = append(invalidPaths, node.Path+" (invalid name)")
		s.log.Warn("Invalid UTF-8 in tree node name",
			slog.String("path", node.Path),
			slog.String("name_bytes", fmt.Sprintf("%q", node.Name)))
	}
	if !utf8.ValidString(node.Path) {
		invalidPaths = append(invalidPaths, node.Path+" (invalid path)")
		s.log.Warn("Invalid UTF-8 in tree node path",
			slog.String("path_bytes", fmt.Sprintf("%q", node.Path)))
	}
	if !utf8.ValidString(node.Codec) {
		invalidPaths = append(invalidPaths, node.Path+" (invalid codec)")
		s.log.Warn("Invalid UTF-8 in tree node codec",
			slog.String("path", node.Path),
			slog.String("codec_bytes", fmt.Sprintf("%q", node.Codec)))
	}
	if !utf8.ValidString(node.Status) {
		invalidPaths = append(invalidPaths, node.Path+" (invalid status)")
		s.log.Warn("Invalid UTF-8 in tree node status",
			slog.String("path", node.Path),
			slog.String("status_bytes", fmt.Sprintf("%q", node.Status)))
	}

	// Recursively check children
	for i := range node.Children {
		childInvalid := s.validateTreeUTF8(&node.Children[i])
		invalidPaths = append(invalidPaths, childInvalid...)
	}

	return invalidPaths
}

// handleQueueDelete handles deletion of a queue item.
func (s *Server) handleQueueDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		http.Error(w, "id parameter is required", http.StatusBadRequest)
		return
	}

	// Parse ID
	var id uint64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "invalid id parameter", http.StatusBadRequest)
		return
	}

	// Check for overflow when converting uint64 to uint
	if id > uint64(^uint(0)) {
		http.Error(w, "id parameter too large", http.StatusBadRequest)
		return
	}

	// Cancel the running process if it's currently being processed
	cancelled := s.queueProc.CancelQueueItem(uint(id))
	if cancelled {
		s.log.Info("Cancelled running transcode process for queue item",
			slog.Uint64("queue_item_id", id))
	}

	// Delete task logs first
	if err := database.DeleteTaskLogsByQueueItem(s.db, uint(id)); err != nil {
		http.Error(w, fmt.Sprintf("failed to delete task logs: %v", err), http.StatusInternalServerError)
		return
	}

	// Delete queue item
	if err := database.DeleteQueueItem(s.db, uint(id)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"message":   "Queue item deleted successfully",
		"cancelled": cancelled,
	}); err != nil {
		s.log.Error("Failed to encode JSON response",
			slog.String("endpoint", "/api/queue/delete"),
			slog.Any("error", err))
	}
}

// handleQueueDeleteBulk handles deletion of multiple queue items.
func (s *Server) handleQueueDeleteBulk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse request body
	var requestBody struct {
		IDs []uint `json:"ids"`
	}

	if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}

	if len(requestBody.IDs) == 0 {
		http.Error(w, "no IDs provided", http.StatusBadRequest)
		return
	}

	// Delete each queue item
	var successCount int
	var cancelledCount int
	var lastError error

	for _, id := range requestBody.IDs {
		// Cancel the running process if it's currently being processed
		if s.queueProc.CancelQueueItem(id) {
			cancelledCount++
			s.log.Info("Cancelled running transcode process for queue item",
				slog.Uint64("queue_item_id", uint64(id)))
		}

		// Delete task logs first
		if err := database.DeleteTaskLogsByQueueItem(s.db, id); err != nil {
			lastError = err
			s.log.Error("Failed to delete task logs",
				slog.Uint64("queue_item_id", uint64(id)),
				slog.Any("error", err))
			continue
		}

		// Delete queue item
		if err := database.DeleteQueueItem(s.db, id); err != nil {
			lastError = err
			s.log.Error("Failed to delete queue item",
				slog.Uint64("queue_item_id", uint64(id)),
				slog.Any("error", err))
			continue
		}

		successCount++
	}

	w.Header().Set("Content-Type", "application/json")
	if successCount == 0 && lastError != nil {
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   false,
			"message":   lastError.Error(),
			"count":     0,
			"cancelled": cancelledCount,
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response",
				slog.String("endpoint", "/api/queue/delete-bulk"),
				slog.Any("error", encodeErr))
		}
		return
	}

	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"count":     successCount,
		"cancelled": cancelledCount,
		"message":   fmt.Sprintf("Deleted %d queue item(s), cancelled %d running process(es)", successCount, cancelledCount),
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response",
			slog.String("endpoint", "/api/queue/delete-bulk"),
			slog.Any("error", encodeErr))
	}
}

// handleQueueLogs returns logs for a specific queue item.
func (s *Server) handleQueueLogs(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		http.Error(w, "id parameter is required", http.StatusBadRequest)
		return
	}

	// Parse ID
	var id uint64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "invalid id parameter", http.StatusBadRequest)
		return
	}

	// Check for overflow when converting uint64 to uint
	if id > uint64(^uint(0)) {
		http.Error(w, "id parameter too large", http.StatusBadRequest)
		return
	}

	// Get logs
	logs, err := database.GetTaskLogsByQueueItem(s.db, uint(id))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(logs); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// renderQueueItemHTML renders a queue item as HTML.
func (s *Server) renderQueueItemHTML(item *database.QueueItem) (string, error) {
	// Ensure File is preloaded
	if item.File == nil {
		var file database.File
		if err := s.db.First(&file, item.FileID).Error; err != nil {
			return "", fmt.Errorf("failed to load file: %w", err)
		}
		item.File = &file
	}

	var buf bytes.Buffer
	if err := s.templates.ExecuteTemplate(&buf, "queue-item.html", item); err != nil {
		return "", fmt.Errorf("failed to render template: %w", err)
	}

	return buf.String(), nil
}

// handleQueueItem returns a single queue item as HTML.
func (s *Server) handleQueueItem(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		http.Error(w, "id parameter required", http.StatusBadRequest)
		return
	}

	var id uint64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	// Check for overflow when converting uint64 to uint
	if id > uint64(^uint(0)) {
		http.Error(w, "id parameter too large", http.StatusBadRequest)
		return
	}

	var item database.QueueItem
	if err := s.db.Preload("File").First(&item, uint(id)).Error; err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	if err := s.templates.ExecuteTemplate(w, "queue-item.html", &item); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// handleProfiles handles GET (list all profiles) and POST (create profile) on /api/profiles.
func (s *Server) handleProfiles(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// List all profiles
		profiles, err := database.GetAllQualityProfiles(s.db)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(profiles); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}

	case http.MethodPost:
		// Create new profile with JSON body
		s.handleProfileCreateJSON(w, r)

	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "Method not allowed",
			"error":   "Only GET and POST methods are supported",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
	}
}

// handleProfileCreateJSON handles POST requests with JSON body to create a new profile.
func (s *Server) handleProfileCreateJSON(w http.ResponseWriter, r *http.Request) {
	// Parse JSON body
	var reqData struct {
		Name              string  `json:"name"`
		Description       string  `json:"description"`
		Mode              string  `json:"mode"`
		TargetBitrateBPS  int64   `json:"target_bitrate_bps"`
		Quality           int     `json:"quality"`
		BitrateMultiplier float64 `json:"bitrate_multiplier"`
		IsDefault         bool    `json:"is_default"`
	}

	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "Invalid JSON body",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Validate required fields
	if reqData.Name == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "name is required",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	if reqData.Mode != "vbr" && reqData.Mode != "cbr" && reqData.Mode != "cbr_percent" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "mode must be 'vbr', 'cbr', or 'cbr_percent'",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Mode-specific validation
	if reqData.Mode == "cbr" && reqData.TargetBitrateBPS <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "target_bitrate_bps must be greater than 0 for CBR mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	if reqData.Mode == "vbr" && reqData.Quality == 0 && reqData.BitrateMultiplier == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "at least one of quality or bitrate_multiplier must be set for VBR mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	if reqData.Mode == "cbr_percent" && reqData.BitrateMultiplier <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "bitrate_multiplier must be greater than 0 for CBR_PERCENT mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Create profile
	profile := &database.QualityProfile{
		ID:                0, // Will be auto-assigned by GORM
		Name:              reqData.Name,
		Description:       reqData.Description,
		Mode:              reqData.Mode,
		TargetBitrate:     reqData.TargetBitrateBPS,
		QualityLevel:      reqData.Quality,
		BitrateMultiplier: reqData.BitrateMultiplier,
		IsDefault:         false,
		CreatedAt:         time.Time{}, // Will be auto-set by GORM
		UpdatedAt:         time.Time{}, // Will be auto-set by GORM
	}

	// Handle is_default flag
	if reqData.IsDefault {
		// Unset other defaults first
		var err error
		if err = s.db.Model(&database.QualityProfile{
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
		}).
			Where("is_default = ?", true).
			Update("is_default", false).Error; err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "failed to unset other defaults",
				"error":   err.Error(),
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
		profile.IsDefault = true
	}

	// Save profile
	var err error
	if err = database.CreateOrUpdateQualityProfile(s.db, profile); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to create profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Broadcast profile created event
	s.broadcaster.BroadcastProfileCreated(profile.ID, profile.Name)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Profile created successfully",
		"profile": profile,
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleProfileGet returns a single profile by ID.
func (s *Server) handleProfileGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.URL.Query().Get("id")
	if idStr == "" {
		http.Error(w, "id parameter is required", http.StatusBadRequest)
		return
	}

	var id uint64
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "invalid id parameter", http.StatusBadRequest)
		return
	}

	// Check for overflow when converting uint64 to uint
	if id > uint64(^uint(0)) {
		http.Error(w, "id parameter too large", http.StatusBadRequest)
		return
	}

	profile, err := database.GetQualityProfileByID(s.db, uint(id))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(profile); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleProfileCreate creates a new quality profile.
func (s *Server) handleProfileCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse form
	name := r.FormValue("name")
	description := r.FormValue("description")
	mode := r.FormValue("mode")
	targetBitrateStr := r.FormValue("target_bitrate")
	qualityLevelStr := r.FormValue("quality_level")
	bitrateMultiplierStr := r.FormValue("bitrate_multiplier")

	// Validate required fields
	if name == "" {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "name is required",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	if mode != "vbr" && mode != "cbr" && mode != "cbr_percent" {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "mode must be 'vbr', 'cbr', or 'cbr_percent'",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Parse numeric fields
	var targetBitrate int64
	var qualityLevel int
	var bitrateMultiplier float64
	var err error

	if targetBitrateStr != "" {
		if _, err = fmt.Sscanf(targetBitrateStr, "%d", &targetBitrate); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid target_bitrate",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
	}

	if qualityLevelStr != "" {
		if _, err = fmt.Sscanf(qualityLevelStr, "%d", &qualityLevel); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid quality_level",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
	}

	if bitrateMultiplierStr != "" {
		if _, err = fmt.Sscanf(bitrateMultiplierStr, "%f", &bitrateMultiplier); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid bitrate_multiplier",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
	}

	// Mode-specific validation
	if mode == "cbr" && targetBitrate <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "target_bitrate must be greater than 0 for CBR mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	if mode == "vbr" && qualityLevel == 0 && bitrateMultiplier == 0 {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "at least one of quality_level or bitrate_multiplier must be set for VBR mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	if mode == "cbr_percent" && bitrateMultiplier <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "bitrate_multiplier must be greater than 0 for CBR_PERCENT mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Create profile
	profile := &database.QualityProfile{
		ID:                0, // Will be auto-assigned by GORM
		Name:              name,
		Description:       description,
		Mode:              mode,
		TargetBitrate:     targetBitrate,
		QualityLevel:      qualityLevel,
		BitrateMultiplier: bitrateMultiplier,
		IsDefault:         false,
		CreatedAt:         time.Time{}, // Will be auto-set by GORM
		UpdatedAt:         time.Time{}, // Will be auto-set by GORM
	}

	if err = database.CreateOrUpdateQualityProfile(s.db, profile); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to create profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Broadcast profile created event
	s.broadcaster.BroadcastProfileCreated(profile.ID, profile.Name)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Profile created successfully",
		"profile": profile,
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleProfileUpdate updates an existing quality profile.
func (s *Server) handleProfileUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse form
	idStr := r.FormValue("id")
	name := r.FormValue("name")
	description := r.FormValue("description")
	mode := r.FormValue("mode")
	targetBitrateStr := r.FormValue("target_bitrate")
	qualityLevelStr := r.FormValue("quality_level")
	bitrateMultiplierStr := r.FormValue("bitrate_multiplier")
	isDefaultStr := r.FormValue("is_default")

	// Validate ID
	if idStr == "" {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "id is required",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	var id uint64
	var err error
	if _, err = fmt.Sscanf(idStr, "%d", &id); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "invalid id",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Check for overflow when converting uint64 to uint
	if id > uint64(^uint(0)) {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "id parameter too large",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Load existing profile
	profile, err := database.GetQualityProfileByID(s.db, uint(id))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "profile not found",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Update fields from form
	if name != "" {
		profile.Name = name
	}
	if description != "" {
		profile.Description = description
	}
	if mode != "" {
		if mode != "vbr" && mode != "cbr" {
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "mode must be 'vbr' or 'cbr'",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
		profile.Mode = mode
	}

	if targetBitrateStr != "" {
		var targetBitrate int64
		if _, err = fmt.Sscanf(targetBitrateStr, "%d", &targetBitrate); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid target_bitrate",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
		profile.TargetBitrate = targetBitrate
	}

	if qualityLevelStr != "" {
		var qualityLevel int
		if _, err = fmt.Sscanf(qualityLevelStr, "%d", &qualityLevel); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid quality_level",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
		profile.QualityLevel = qualityLevel
	}

	if bitrateMultiplierStr != "" {
		var bitrateMultiplier float64
		if _, err = fmt.Sscanf(bitrateMultiplierStr, "%f", &bitrateMultiplier); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid bitrate_multiplier",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
		profile.BitrateMultiplier = bitrateMultiplier
	}

	// Handle is_default flag
	switch isDefaultStr {
	case "true":
		// Unset other defaults first
		if err = s.db.Model(&database.QualityProfile{
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
		}).
			Where("is_default = ?", true).
			Update("is_default", false).Error; err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "failed to unset other defaults",
				"error":   err.Error(),
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
		profile.IsDefault = true
	case "false":
		profile.IsDefault = false
	}

	// Mode-specific validation
	if profile.Mode == "cbr" && profile.TargetBitrate <= 0 {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "target_bitrate must be greater than 0 for CBR mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	if profile.Mode == "vbr" && profile.QualityLevel == 0 && profile.BitrateMultiplier == 0 {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "at least one of quality_level or bitrate_multiplier must be set for VBR mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Save profile
	if err = database.CreateOrUpdateQualityProfile(s.db, profile); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to update profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Broadcast profile updated event
	s.broadcaster.BroadcastProfileUpdated(profile.ID, profile.Name)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Profile updated successfully",
		"profile": profile,
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleProfileDelete deletes a quality profile.
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idStr := r.FormValue("id")
	if idStr == "" {
		idStr = r.URL.Query().Get("id")
	}

	if idStr == "" {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "id is required",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	var id uint64
	var err error
	if _, err = fmt.Sscanf(idStr, "%d", &id); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "invalid id",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Check for overflow when converting uint64 to uint
	if id > uint64(^uint(0)) {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "id parameter too large",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Delete profile
	if err = database.DeleteQualityProfile(s.db, uint(id)); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to delete profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Broadcast profile deleted event
	s.broadcaster.BroadcastProfileDeleted(uint(id))

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Profile deleted successfully",
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleProfileByID handles RESTful requests to /api/profiles/{id}.
// Supports PUT (update) and DELETE (delete) methods with JSON bodies.
func (s *Server) handleProfileByID(w http.ResponseWriter, r *http.Request) {
	// Extract ID from URL path (e.g., /api/profiles/123 -> 123)
	pathPrefix := "/api/profiles/"
	if !strings.HasPrefix(r.URL.Path, pathPrefix) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, pathPrefix)
	// Remove any trailing path segments (e.g., /api/profiles/123/default -> 123)
	if idx := strings.Index(idStr, "/"); idx != -1 {
		idStr = idStr[:idx]
	}

	if idStr == "" {
		http.Error(w, "Profile ID is required", http.StatusBadRequest)
		return
	}

	var id uint64
	var err error
	if _, err = fmt.Sscanf(idStr, "%d", &id); err != nil {
		http.Error(w, "Invalid profile ID", http.StatusBadRequest)
		return
	}

	// Check for overflow when converting uint64 to uint
	if id > uint64(^uint(0)) {
		http.Error(w, "Profile ID too large", http.StatusBadRequest)
		return
	}

	profileID := uint(id)

	// Route based on HTTP method
	switch r.Method {
	case http.MethodPut:
		s.handleProfileUpdateJSON(w, r, profileID)
	case http.MethodDelete:
		s.handleProfileDeleteByID(w, profileID)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleProfileUpdateJSON handles PUT requests with JSON body to update a profile.
func (s *Server) handleProfileUpdateJSON(w http.ResponseWriter, r *http.Request, profileID uint) {
	// Parse JSON body
	var reqData struct {
		Name              string  `json:"name"`
		Description       string  `json:"description"`
		Mode              string  `json:"mode"`
		TargetBitrateBPS  int64   `json:"target_bitrate_bps"`
		Quality           int     `json:"quality"`
		BitrateMultiplier float64 `json:"bitrate_multiplier"`
		IsDefault         bool    `json:"is_default"`
	}

	if err := json.NewDecoder(r.Body).Decode(&reqData); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "Invalid JSON body",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Load existing profile
	profile, err := database.GetQualityProfileByID(s.db, profileID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "Profile not found",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Update fields
	if reqData.Name != "" {
		profile.Name = reqData.Name
	}
	profile.Description = reqData.Description
	if reqData.Mode != "" {
		if reqData.Mode != "vbr" && reqData.Mode != "cbr" && reqData.Mode != "cbr_percent" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "mode must be 'vbr', 'cbr', or 'cbr_percent'",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
		profile.Mode = reqData.Mode
	}

	profile.TargetBitrate = reqData.TargetBitrateBPS
	profile.QualityLevel = reqData.Quality
	profile.BitrateMultiplier = reqData.BitrateMultiplier

	// Handle is_default flag
	if reqData.IsDefault {
		// Unset other defaults first
		if err = s.db.Model(&database.QualityProfile{
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
		}).
			Where("is_default = ?", true).
			Update("is_default", false).Error; err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "failed to unset other defaults",
				"error":   err.Error(),
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return
		}
	}
	profile.IsDefault = reqData.IsDefault

	// Mode-specific validation
	if profile.Mode == "cbr" && profile.TargetBitrate <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "target_bitrate_bps must be greater than 0 for CBR mode",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Save profile
	if err = database.CreateOrUpdateQualityProfile(s.db, profile); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to update profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Broadcast profile updated event
	s.broadcaster.BroadcastProfileUpdated(profile.ID, profile.Name)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Profile updated successfully",
		"profile": profile,
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleProfileDeleteByID handles DELETE requests to delete a profile.
func (s *Server) handleProfileDeleteByID(w http.ResponseWriter, profileID uint) {
	// Delete profile
	if err := database.DeleteQualityProfile(s.db, profileID); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to delete profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Broadcast profile deleted event
	s.broadcaster.BroadcastProfileDeleted(profileID)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Profile deleted successfully",
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// parseQualityProfileID parses and validates a quality_profile_id from query string.
// Returns the parsed ID (0 if empty) and true if valid, or false with error response written.
func (s *Server) parseQualityProfileID(w http.ResponseWriter, qualityProfileIDStr string) (uint64, bool) {
	var qualityProfileID uint64
	var err error

	if qualityProfileIDStr != "" {
		if _, err = fmt.Sscanf(qualityProfileIDStr, "%d", &qualityProfileID); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "invalid quality_profile_id",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return 0, false
		}

		// Check for overflow when converting uint64 to uint
		if qualityProfileID > uint64(^uint(0)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "quality_profile_id too large",
				"error":   "validation error",
			}); encodeErr != nil {
				s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
			}
			return 0, false
		}
	}

	return qualityProfileID, true
}

// handleSetFileQualityProfile handles POST requests to set a file's preferred quality profile.
func (s *Server) handleSetFileQualityProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse form values
	fileIDStr := r.FormValue("file_id")
	qualityProfileIDStr := r.FormValue("quality_profile_id")

	if fileIDStr == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "file_id is required",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Parse file ID
	var fileID uint64
	var err error
	if _, err = fmt.Sscanf(fileIDStr, "%d", &fileID); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "invalid file_id",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Check for overflow when converting uint64 to uint
	if fileID > uint64(^uint(0)) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "file_id too large",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Parse quality profile ID (defaults to 0 if empty, which means "use default")
	qualityProfileID, ok := s.parseQualityProfileID(w, qualityProfileIDStr)
	if !ok {
		return
	}

	// Update the file's quality profile preference
	if err = database.UpdateFileQualityProfile(s.db, uint(fileID), uint(qualityProfileID)); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to update file quality profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "File quality profile updated successfully",
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// handleSetFolderQualityProfile handles POST requests to set a folder's quality profile (cascading to all files below).
func (s *Server) handleSetFolderQualityProfile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse form values
	folderPath := r.FormValue("folder_path")
	qualityProfileIDStr := r.FormValue("quality_profile_id")

	if folderPath == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "folder_path is required",
			"error":   "validation error",
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Parse quality profile ID (defaults to 0 if empty, which means "use default")
	qualityProfileID, ok := s.parseQualityProfileID(w, qualityProfileIDStr)
	if !ok {
		return
	}

	var err error
	// Update the folder's quality profile (cascades to all files below)
	if err = database.UpdateFolderQualityProfile(s.db, folderPath, uint(qualityProfileID)); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to update folder quality profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
		}
		return
	}

	// Broadcast folder profile changed event
	var profileName string
	if qualityProfileID > 0 {
		profile, profileErr := database.GetQualityProfileByID(s.db, uint(qualityProfileID))
		if profileErr == nil {
			profileName = profile.Name
		}
	}
	s.broadcaster.BroadcastFolderProfileChanged(folderPath, uint(qualityProfileID), profileName)

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Folder quality profile updated successfully",
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

// FilteredFileItem represents a file with its media info for the filtered files API.
type FilteredFileItem struct {
	ID               uint    `json:"id"`
	Path             string  `json:"path"`
	Name             string  `json:"name"`
	Size             int64   `json:"size"`
	Status           string  `json:"status"`
	QualityProfileID uint    `json:"quality_profile_id"`
	Codec            string  `json:"codec,omitempty"`
	Bitrate          int64   `json:"bitrate,omitempty"`
	Width            int     `json:"width,omitempty"`
	Height           int     `json:"height,omitempty"`
	Duration         float64 `json:"duration,omitempty"`
}

func (s *Server) handleFilesFiltered(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse query parameters
	query := r.URL.Query()

	var filters database.FileFilters
	var err error

	// Needs convert filter
	filters.NeedsConvert = query.Get("needs_convert") == "true"

	// Bitrate filters (convert from Mbps to bps)
	if minBitrateStr := query.Get("min_bitrate"); minBitrateStr != "" {
		var minBitrateMbps float64
		if _, scanErr := fmt.Sscanf(minBitrateStr, "%f", &minBitrateMbps); scanErr == nil {
			filters.MinBitrate = int64(minBitrateMbps * bitsPerMegabit)
		}
	}
	if maxBitrateStr := query.Get("max_bitrate"); maxBitrateStr != "" {
		var maxBitrateMbps float64
		if _, scanErr := fmt.Sscanf(maxBitrateStr, "%f", &maxBitrateMbps); scanErr == nil {
			filters.MaxBitrate = int64(maxBitrateMbps * bitsPerMegabit)
		}
	}

	// Size filters (convert from GB to bytes)
	if minSizeStr := query.Get("min_size"); minSizeStr != "" {
		var minSizeGB float64
		if _, scanErr := fmt.Sscanf(minSizeStr, "%f", &minSizeGB); scanErr == nil {
			filters.MinSize = int64(minSizeGB * bytesPerGiB)
		}
	}
	if maxSizeStr := query.Get("max_size"); maxSizeStr != "" {
		var maxSizeGB float64
		if _, scanErr := fmt.Sscanf(maxSizeStr, "%f", &maxSizeGB); scanErr == nil {
			filters.MaxSize = int64(maxSizeGB * bytesPerGiB)
		}
	}

	// Source directory filter
	filters.SourceDir = query.Get("source_dir")

	// Codec filter
	filters.Codec = query.Get("codec")

	// Get filtered files from database
	files, err := database.GetFilteredFiles(s.db, filters)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Build response with media info (preallocate for known size)
	response := make([]FilteredFileItem, 0, len(files))
	for i := range files {
		item := FilteredFileItem{
			ID:               files[i].ID,
			Path:             files[i].Path,
			Name:             files[i].Name,
			Size:             files[i].Size,
			Status:           files[i].Status,
			QualityProfileID: files[i].QualityProfileID,
			Codec:            "",
			Bitrate:          0,
			Width:            0,
			Height:           0,
			Duration:         0,
		}

		// Get media info for this file
		var mediaInfo *database.MediaInfo
		mediaInfo, err = database.GetMediaInfoByFileID(s.db, files[i].ID)
		if err == nil {
			item.Codec = mediaInfo.VideoCodec
			item.Bitrate = mediaInfo.VideoBitrate
			item.Width = mediaInfo.Width
			item.Height = mediaInfo.Height
			item.Duration = mediaInfo.Duration
		}

		response = append(response, item)
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(response); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}

func (s *Server) handleBulkConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse JSON request body
	var request struct {
		FileIDs          []uint `json:"file_ids"`
		QualityProfileID uint   `json:"quality_profile_id"`
	}

	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if len(request.FileIDs) == 0 {
		http.Error(w, "No file IDs provided", http.StatusBadRequest)
		return
	}

	// Determine which profile to use (0 = use file's assigned profile or default)
	profileID := request.QualityProfileID
	if profileID == 0 {
		// Get default profile
		var defaultProfile *database.QualityProfile
		var err error
		defaultProfile, err = database.GetDefaultQualityProfile(s.db)
		if err != nil {
			http.Error(w, "No default quality profile found", http.StatusInternalServerError)
			return
		}
		profileID = defaultProfile.ID
	}

	// Add each file to the queue
	successCount := 0
	for _, fileID := range request.FileIDs {
		// Get file to check if it exists
		var file database.File
		result := s.db.Where("id = ?", fileID).First(&file)
		if result.Error != nil {
			continue
		}

		// Add to queue using file path
		var queueItemID uint
		var err error
		queueItemID, err = s.queueProc.AddFileToQueue(file.Path, profileID)
		if err == nil {
			successCount++
			// Broadcast event for each file
			s.broadcaster.BroadcastQueueAdded(queueItemID, file.Path)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   successCount,
		"message": fmt.Sprintf("Added %d file(s) to queue", successCount),
	}); encodeErr != nil {
		s.log.Error("Failed to encode JSON response", slog.Any("error", encodeErr))
	}
}
