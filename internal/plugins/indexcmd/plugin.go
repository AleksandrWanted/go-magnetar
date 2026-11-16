package indexcmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wmentor/go-magnetar/internal/agent/indexer"
	"github.com/wmentor/go-magnetar/internal/common"
	"github.com/wmentor/go-magnetar/internal/plugin"
	"github.com/wmentor/go-magnetar/internal/printer"
)

func init() {
	plugin.Register("indexcmd", &Plugin{})
}

type Plugin struct {
	idx *indexer.Indexer
}

// activeIndexer holds the indexer created during plugin init. Batch mode
// (go-magnetar --idxtab <file>) reuses it so both code paths share one store handle.
var activeIndexer *indexer.Indexer

// Indexer returns the plugin's indexer, or nil if the plugin did not init
// (rag disabled or misconfigured).
func Indexer() *indexer.Indexer {
	return activeIndexer
}

func (p *Plugin) Init(s *plugin.State, hub plugin.Hub) error {
	if !s.Config.Bool("rag.enable") || s.Config.String("rag.llm.base_url") == "" {
		return nil
	}

	root, err := os.OpenRoot("/")
	if err != nil {
		return fmt.Errorf("indexcmd: failed to open root: %w", err)
	}
	defer root.Close()

	p.idx, err = indexer.New(s.Config, root, s)
	if err != nil {
		return fmt.Errorf("indexcmd: failed to create indexer: %w", err)
	}

	activeIndexer = p.idx

	hub.RegisterChatCommand(plugin.ChatCommand{
		Name:    "index",
		Aliases: []string{"i"},
		Help:    "Index a file, URL, or directory into RAG knowledge base (use `-r` for directories)",
		Execute: p.execute,
	})

	hub.RegisterChatCommand(plugin.ChatCommand{
		Name:    "idxtab",
		Aliases: []string{},
		Help:    "Index multiple files/URLs from a JSON lines file (one per line, format: {\"source\":\"path|url\",\"message\":\"text\"})",
		Execute: p.executeTab,
	})

	return nil
}

func (p *Plugin) execute(ctx context.Context, params *plugin.ChatCommandInput) (*plugin.ChatCommandResponse, error) {
	args := params.Args
	if args == "" {
		return nil, fmt.Errorf("usage: /index <path|url> [-r|--recursive] [-m <message>]")
	}

	opts, err := parseIndexArgs(args)
	if err != nil {
		return nil, fmt.Errorf("parse index arg error: %w", err)
	}

	switch {
	case isURL(opts.target):
		if err := p.idx.IndexURL(opts.target, opts.message); err != nil {
			return nil, fmt.Errorf("index URL failed: %w", err)
		}
		return plugin.NopChatCommandResponse, nil
	case isDir(opts.target):
		if !opts.recursive {
			return nil, fmt.Errorf("target is a directory, use -r or --recursive for recursive indexing")
		}

		err := p.idx.IndexDirectory(opts.target, opts.message)
		if err != nil {
			return nil, fmt.Errorf("index directory failed: %w", err)
		}
		return plugin.NopChatCommandResponse, nil
	default:
		if err := p.idx.IndexFile(opts.target, opts.message); err != nil {
			return nil, fmt.Errorf("index file failed: %w", err)
		}
	}

	return plugin.NopChatCommandResponse, nil
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func parseIndexArgs(args string) (IndexArgs, error) {
	parts := strings.Fields(args)
	opts := IndexArgs{}

	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "-m":
			if i+1 < len(parts) {
				opts.message = parts[i+1]
				i++
			} else {
				return IndexArgs{}, fmt.Errorf("message can not be empty")
			}
		case "-r", "--recursive":
			opts.recursive = true
		default:
			if strings.HasPrefix(parts[i], "-") {
				return IndexArgs{}, fmt.Errorf("unknown flag: %q", parts[i])
			}
			if opts.target != "" {
				return IndexArgs{}, fmt.Errorf("too many targets: %q and %q", opts.target, parts[i])
			}
			opts.target = parts[i]
		}
	}

	if opts.target == "" {
		return IndexArgs{}, fmt.Errorf("target not found")
	}
	return opts, nil
}

type IndexArgs struct {
	target    string
	message   string
	recursive bool
}

type Entry struct {
	Source  string `json:"source"`
	Message string `json:"message"`
}

// indexEntry indexes a single JSONL entry: URL via fetch, otherwise file path
// (relative paths are resolved against baseDir).
func indexEntry(idx *indexer.Indexer, entry Entry, baseDir string) error {
	source := entry.Source

	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return idx.IndexURL(source, entry.Message)
	}

	if !filepath.IsAbs(source) {
		source = filepath.Join(baseDir, source)
	}
	return idx.IndexFile(source, entry.Message)
}

// ProcessJSONL batch-indexes a JSONL file into the RAG store. Lines may be
// empty or start with '#'. Returns the number of successfully indexed entries
// and the number of errors.
func ProcessJSONL(idx *indexer.Indexer, filename string) (int, int, error) {
	filename = common.ExpandHome(filename)

	file, err := os.Open(filename)
	if err != nil {
		return 0, 0, fmt.Errorf("open file error: %w", err)
	}
	defer file.Close()

	// Relative sources inside the JSONL resolve against the JSONL's directory.
	baseDir := filepath.Dir(filename)

	scanner := bufio.NewScanner(file)
	count, failed := 0, 0

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		var entry Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			printer.Error("json parse error", "line", line, "error", err)
			failed++
			continue
		}

		if err := indexEntry(idx, entry, baseDir); err != nil {
			printer.Error("index entry failed", "source", entry.Source, "error", err)
			failed++
			continue
		}

		count++
	}

	if err := scanner.Err(); err != nil {
		return count, failed, fmt.Errorf("scan error: %w", err)
	}

	return count, failed, nil
}

func (p *Plugin) executeTab(ctx context.Context, params *plugin.ChatCommandInput) (*plugin.ChatCommandResponse, error) {
	args := params.Args
	filename := strings.TrimSpace(args)

	count, failed, err := ProcessJSONL(p.idx, filename)
	if err != nil {
		return nil, err
	}

	printer.Print(printer.IconDone, fmt.Sprintf("idxtab: done, %d indexed, %d failed", count, failed))

	return plugin.NopChatCommandResponse, nil
}
