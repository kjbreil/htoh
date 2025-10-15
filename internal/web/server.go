package web

import (
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

	"gorm.io/gorm"
	"opti.local/opti/internal/runner"
)

// Server represents the web server
type Server struct {
	db          *gorm.DB
	cfg         runner.Config
	broadcaster *EventBroadcaster
	queueProc   *runner.QueueProcessor
	templates   *template.Template
}

// NewServer creates a new web server
func NewServer(db *gorm.DB, cfg runner.Config, queueProc *runner.QueueProcessor) (*Server, error) {
	broadcaster := NewEventBroadcaster()

	// Parse templates
	tmpl, err := template.New("").Funcs(template.FuncMap{
		"formatSize": formatSize,
		"formatCodec": formatCodec,
	}).ParseGlob(filepath.Join("internal", "web", "templates", "*.html"))
	if err != nil {
		return nil, fmt.Errorf("failed to parse templates: %w", err)
	}

	return &Server{
		db:          db,
		cfg:         cfg,
		broadcaster: broadcaster,
		queueProc:   queueProc,
		templates:   tmpl,
	}, nil
}

// Start starts the HTTP server
func (s *Server) Start(ctx context.Context, port int) error {
	mux := http.NewServeMux()

	// Routes
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/api/tree", s.handleTree)
	mux.HandleFunc("/api/convert", s.handleConvert)
	mux.HandleFunc("/api/events", s.handleEvents)
	mux.HandleFunc("/api/queue", s.handleQueue)

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Graceful shutdown
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdownCtx)
	}()

	fmt.Printf("Web server starting on http://%s\n", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("server error: %w", err)
	}

	return nil
}

// handleIndex serves the main page
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if err := s.templates.ExecuteTemplate(w, "index.html", nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// TreeNode represents a node in the directory tree
type TreeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	IsDir    bool        `json:"is_dir"`
	FileID   uint        `json:"file_id,omitempty"`
	Codec    string      `json:"codec,omitempty"`
	Size     int64       `json:"size,omitempty"`
	Status   string      `json:"status,omitempty"`
	Children []TreeNode  `json:"children,omitempty"`
	Stats    *FolderStats `json:"stats,omitempty"`
}

// FolderStats represents aggregate statistics for a folder
type FolderStats struct {
	TotalFiles int              `json:"total_files"`
	TotalSize  int64            `json:"total_size"`
	CodecCount map[string]int   `json:"codec_count"`
	H264Count  int              `json:"h264_count"`
}

// handleTree returns the directory tree structure
func (s *Server) handleTree(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = s.cfg.SourceDir
	}

	// Build tree
	tree, err := s.buildTree(path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(tree)
}

// buildTree builds a directory tree structure
func (s *Server) buildTree(rootPath string) (*TreeNode, error) {
	// Get file info
	info, err := os.Stat(rootPath)
	if err != nil {
		return nil, err
	}

	root := &TreeNode{
		Name:  filepath.Base(rootPath),
		Path:  rootPath,
		IsDir: info.IsDir(),
	}

	if !info.IsDir() {
		// Single file
		file, err := runner.GetFileByPath(s.db, rootPath)
		if err == nil {
			root.FileID = file.ID
			root.Size = file.Size
			root.Status = file.Status

			mediaInfo, err := runner.GetMediaInfoByFileID(s.db, file.ID)
			if err == nil {
				root.Codec = mediaInfo.VideoCodec
			}
		}
		return root, nil
	}

	// Directory - get all files from database
	var files []runner.File
	if err := s.db.Where("path LIKE ?", rootPath+"%").Find(&files).Error; err != nil {
		return nil, err
	}

	// Build file map
	fileMap := make(map[string]*runner.File)
	for i := range files {
		fileMap[files[i].Path] = &files[i]
	}

	// Walk directory structure
	entries, err := os.ReadDir(rootPath)
	if err != nil {
		return nil, err
	}

	// Process entries
	for _, entry := range entries {
		childPath := filepath.Join(rootPath, entry.Name())

		if entry.IsDir() {
			// Recursive for directories
			childNode, err := s.buildTree(childPath)
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

				mediaInfo, err := runner.GetMediaInfoByFileID(s.db, file.ID)
				if err == nil {
					childNode.Codec = mediaInfo.VideoCodec
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

// calculateFolderStats calculates aggregate statistics for a folder
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

// handleConvert handles conversion requests
func (s *Server) handleConvert(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	path := r.FormValue("path")
	isDir := r.FormValue("is_dir") == "true"

	if path == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}

	var count int
	var err error

	if isDir {
		count, err = s.queueProc.AddFolderToQueue(path)
	} else {
		err = s.queueProc.AddFileToQueue(path)
		if err == nil {
			count = 1
		}
	}

	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Broadcast event
	s.broadcaster.BroadcastQueueAdded(0, path)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   count,
		"message": fmt.Sprintf("Added %d file(s) to queue", count),
	})
}

// handleEvents handles SSE connections
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

// handleQueue returns the current queue status
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")

	items, err := runner.GetQueueItems(s.db, status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(items)
}

// formatSize formats a byte size as human-readable string
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

// formatCodec formats a codec name for display
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
