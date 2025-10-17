package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kjbreil/opti/internal/config"
	"github.com/kjbreil/opti/internal/database"
	"github.com/kjbreil/opti/internal/queue"
	"github.com/kjbreil/opti/internal/scanner"
	"gorm.io/gorm"
)

// Server represents the web server.
type Server struct {
	db          *gorm.DB
	cfg         config.Config
	broadcaster *EventBroadcaster
	queueProc   *queue.QueueProcessor
	templates   *template.Template
}

// NewServer creates a new web server.
func NewServer(db *gorm.DB, cfg config.Config, queueProc *queue.QueueProcessor) (*Server, error) {
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
	}

	// Set HTML broadcaster for queue item status updates
	queueProc.SetHTMLBroadcaster(func(queueItemID uint) {
		var item database.QueueItem
		var loadErr error
		if loadErr = db.Preload("File").First(&item, queueItemID).Error; loadErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to load queue item for HTML render: %v\n", loadErr)
			return
		}

		var html string
		var renderErr error
		html, renderErr = s.renderQueueItemHTML(&item)
		if renderErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to render queue item HTML: %v\n", renderErr)
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
	mux.HandleFunc("/api/queue/logs", s.handleQueueLogs)

	// Quality profile routes
	mux.HandleFunc("/api/profiles", s.handleProfiles)
	mux.HandleFunc("/api/profiles/get", s.handleProfileGet)
	mux.HandleFunc("/api/profiles/create", s.handleProfileCreate)
	mux.HandleFunc("/api/profiles/update", s.handleProfileUpdate)
	mux.HandleFunc("/api/profiles/delete", s.handleProfileDelete)
	// RESTful routes with ID in path (must come after exact matches)
	mux.HandleFunc("/api/profiles/", s.handleProfileByID)

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "Server shutdown error: %v\n", err)
		}
	}()

	fmt.Printf("Web server starting on http://%s\n", addr)
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
	Name       string       `json:"name"`
	Path       string       `json:"path"`
	IsDir      bool         `json:"is_dir"`
	FileID     uint         `json:"file_id,omitempty"`
	Codec      string       `json:"codec,omitempty"`
	Size       int64        `json:"size,omitempty"`
	Status     string       `json:"status,omitempty"`
	CanConvert bool         `json:"can_convert,omitempty"`
	Children   []TreeNode   `json:"children,omitempty"`
	Stats      *FolderStats `json:"stats,omitempty"`
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

	if path == "" {
		// No specific path - return tree(s) for all source directories
		if len(s.cfg.SourceDirs) == 0 {
			http.Error(w, "No source directories configured", http.StatusInternalServerError)
			return
		}

		if len(s.cfg.SourceDirs) == 1 {
			// Single source directory - return single TreeNode for backward compatibility
			tree, err := s.buildTree(s.cfg.SourceDirs[0])
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(tree); encodeErr != nil {
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
			}
		} else {
			// Multiple source directories - return array of TreeNode
			var trees []TreeNode
			for _, sourceDir := range s.cfg.SourceDirs {
				tree, err := s.buildTree(sourceDir)
				if err != nil {
					// Log error but continue with other directories
					continue
				}
				trees = append(trees, *tree)
			}
			w.Header().Set("Content-Type", "application/json")
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(trees); encodeErr != nil {
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
			}
		}
	} else {
		// Specific path provided - return single TreeNode
		tree, err := s.buildTree(path)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(tree); encodeErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
		}
	}
}

// buildTree builds a directory tree structure.
func (s *Server) buildTree(rootPath string) (*TreeNode, error) {
	// Get default quality profile for convertibility checks
	defaultProfile, err := database.GetDefaultQualityProfile(s.db)
	if err != nil {
		// If no default profile exists, log it but continue (all files will be non-convertible)
		fmt.Fprintf(os.Stderr, "Warning: no default quality profile found: %v\n", err)
	}

	// Get file info
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat path %s: %w", rootPath, err)
	}

	root := &TreeNode{
		Name:  filepath.Base(rootPath),
		Path:  rootPath,
		IsDir: info.IsDir(),
	}

	if !info.IsDir() {
		// Single file
		var file *database.File
		file, err = database.GetFileByPath(s.db, rootPath)
		if err == nil {
			root.FileID = file.ID
			root.Size = file.Size
			root.Status = file.Status

			var mediaInfo *database.MediaInfo
			mediaInfo, err = database.GetMediaInfoByFileID(s.db, file.ID)
			if err == nil {
				root.Codec = mediaInfo.VideoCodec
				// Check if file is convertible with default profile
				if defaultProfile != nil {
					root.CanConvert = database.IsEligibleForConversionWithProfile(mediaInfo, defaultProfile)
				}
			}
		}
		return root, nil
	}

	// Directory - get all files from database
	var files []database.File
	var dbErr error
	if dbErr = s.db.Where("path LIKE ?", rootPath+"%").Find(&files).Error; dbErr != nil {
		return nil, dbErr
	}

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

	// Process entries
	for _, entry := range entries {
		childPath := filepath.Join(rootPath, entry.Name())

		if entry.IsDir() {
			// Recursive for directories
			var childNode *TreeNode
			childNode, err = s.buildTree(childPath)
			if err != nil {
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
				Name:  entry.Name(),
				Path:  childPath,
				IsDir: false,
			}

			if file, ok := fileMap[childPath]; ok {
				childNode.FileID = file.ID
				childNode.Size = file.Size
				childNode.Status = file.Status

				var mediaInfo *database.MediaInfo
				mediaInfo, err = database.GetMediaInfoByFileID(s.db, file.ID)
				if err == nil {
					childNode.Codec = mediaInfo.VideoCodec
					// Check if file is convertible with default profile
					if defaultProfile != nil {
						childNode.CanConvert = database.IsEligibleForConversionWithProfile(mediaInfo, defaultProfile)
					}
				}
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

	return root, nil
}

// calculateFolderStats calculates aggregate statistics for a folder.
func (s *Server) calculateFolderStats(children *[]TreeNode) *FolderStats {
	stats := &FolderStats{
		CodecCount: make(map[string]int),
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
	fileCount, err = scanner.ScanOnce(r.Context(), s.cfg, s.db)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
	}
}

// handleEvents handles SSE connections.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Create client channel
	client := make(chan Event, 10)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
	}
}

// formatSize formats a byte size as human-readable string.
func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
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
		"success": true,
		"message": "Queue item deleted successfully",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
		}
		return
	}

	// Create profile
	profile := &database.QualityProfile{
		Name:              reqData.Name,
		Description:       reqData.Description,
		Mode:              reqData.Mode,
		TargetBitrate:     reqData.TargetBitrateBPS,
		QualityLevel:      reqData.Quality,
		BitrateMultiplier: reqData.BitrateMultiplier,
		IsDefault:         false,
	}

	// Handle is_default flag
	if reqData.IsDefault {
		// Unset other defaults first
		var err error
		if err = s.db.Model(&database.QualityProfile{}).
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
		}
		return
	}

	// Create profile
	profile := &database.QualityProfile{
		Name:              name,
		Description:       description,
		Mode:              mode,
		TargetBitrate:     targetBitrate,
		QualityLevel:      qualityLevel,
		BitrateMultiplier: bitrateMultiplier,
		IsDefault:         false,
	}

	if err = database.CreateOrUpdateQualityProfile(s.db, profile); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to create profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
			}
			return
		}
		profile.BitrateMultiplier = bitrateMultiplier
	}

	// Handle is_default flag
	if isDefaultStr == "true" {
		// Unset other defaults first
		if err = s.db.Model(&database.QualityProfile{}).
			Where("is_default = ?", true).
			Update("is_default", false).Error; err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			var encodeErr error
			if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "failed to unset other defaults",
				"error":   err.Error(),
			}); encodeErr != nil {
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
			}
			return
		}
		profile.IsDefault = true
	} else if isDefaultStr == "false" {
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		s.handleProfileDeleteByID(w, r, profileID)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		if err = s.db.Model(&database.QualityProfile{}).
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
				fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
	}
}

// handleProfileDeleteByID handles DELETE requests to delete a profile.
func (s *Server) handleProfileDeleteByID(w http.ResponseWriter, r *http.Request, profileID uint) {
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
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
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
	}
}

// handleProfileSetDefault handles POST requests to set a profile as default.
func (s *Server) handleProfileSetDefault(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Extract ID from URL path (e.g., /api/profiles/123/default -> 123)
	pathPrefix := "/api/profiles/"
	pathSuffix := "/default"
	if !strings.HasPrefix(r.URL.Path, pathPrefix) || !strings.HasSuffix(r.URL.Path, pathSuffix) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	idStr := strings.TrimPrefix(r.URL.Path, pathPrefix)
	idStr = strings.TrimSuffix(idStr, pathSuffix)

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

	// Load profile
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
		}
		return
	}

	// Unset other defaults first
	if err = s.db.Model(&database.QualityProfile{}).
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
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
		}
		return
	}

	// Set this profile as default
	profile.IsDefault = true
	if err = database.CreateOrUpdateQualityProfile(s.db, profile); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		var encodeErr error
		if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"message": "failed to set default profile",
			"error":   err.Error(),
		}); encodeErr != nil {
			fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	var encodeErr error
	if encodeErr = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Default profile updated successfully",
	}); encodeErr != nil {
		fmt.Fprintf(os.Stderr, "Failed to encode JSON response: %v\n", encodeErr)
	}
}
