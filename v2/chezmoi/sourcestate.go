package chezmoi

// FIXME accumulate all source state warnings/errors
// FIXME encryption
// FIXME templates

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/coreos/go-semver/semver"
	vfs "github.com/twpayne/go-vfs"
	"go.uber.org/multierr"
)

// A SourceState is a source state.
type SourceState struct {
	s               System
	sourcePath      string
	umask           os.FileMode
	entries         map[string]SourceStateEntry
	ignore          *PatternSet
	minVersion      *semver.Version
	remove          *PatternSet
	templateData    interface{}
	templateFuncs   template.FuncMap
	templateOptions []string
	templates       map[string]*template.Template
}

// A SourceStateOption sets an option on a source state.
type SourceStateOption func(*SourceState)

// WithSystem sets the system.
func WithSystem(s System) SourceStateOption {
	return func(ss *SourceState) {
		ss.s = s
	}
}

// WithSourcePath sets the source path.
func WithSourcePath(sourcePath string) SourceStateOption {
	return func(ss *SourceState) {
		ss.sourcePath = sourcePath
	}
}

// WithTemplateData sets the template data.
func WithTemplateData(templateData interface{}) SourceStateOption {
	return func(ss *SourceState) {
		ss.templateData = templateData
	}
}

// WithTemplateFuncs sets the template functions.
func WithTemplateFuncs(templateFuncs template.FuncMap) SourceStateOption {
	return func(ss *SourceState) {
		ss.templateFuncs = templateFuncs
	}
}

// WithTemplateOptions sets the template options.
func WithTemplateOptions(templateOptions []string) SourceStateOption {
	return func(ss *SourceState) {
		ss.templateOptions = templateOptions
	}
}

// WithUmask sets the umask.
func WithUmask(umask os.FileMode) SourceStateOption {
	return func(ss *SourceState) {
		ss.umask = umask
	}
}

// NewSourceState creates a new source state with the given options.
func NewSourceState(options ...SourceStateOption) *SourceState {
	ss := &SourceState{
		umask:           0o22,
		entries:         make(map[string]SourceStateEntry),
		ignore:          NewPatternSet(),
		remove:          NewPatternSet(),
		templateOptions: DefaultTemplateOptions,
	}
	for _, option := range options {
		option(ss)
	}
	return ss
}

// Add adds sourceStateEntry to ss.
func (ss *SourceState) Add() error {
	return nil // FIXME
}

// ApplyAll updates targetDir in fs to match ss.
func (ss *SourceState) ApplyAll(s System, umask os.FileMode, targetDir string) error {
	for _, targetName := range ss.sortedTargetNames() {
		if err := ss.ApplyOne(s, umask, targetDir, targetName); err != nil {
			return err
		}
	}
	return nil
}

// ApplyOne updates targetName in targetDir on fs to match ss using s.
func (ss *SourceState) ApplyOne(s System, umask os.FileMode, targetDir, targetName string) error {
	targetPath := path.Join(targetDir, targetName)
	destStateEntry, err := NewDestStateEntry(s, targetPath)
	if err != nil {
		return err
	}
	targetStateEntry, err := ss.entries[targetName].TargetStateEntry()
	if err != nil {
		return err
	}
	if err := targetStateEntry.Apply(s, destStateEntry); err != nil {
		return err
	}
	if targetStateDir, ok := targetStateEntry.(*TargetStateDir); ok {
		if targetStateDir.exact {
			infos, err := s.ReadDir(targetPath)
			if err != nil {
				return err
			}
			baseNames := make([]string, 0, len(infos))
			for _, info := range infos {
				if baseName := info.Name(); baseName != "." && baseName != ".." {
					baseNames = append(baseNames, baseName)
				}
			}
			sort.Strings(baseNames)
			for _, baseName := range baseNames {
				if _, ok := ss.entries[path.Join(targetName, baseName)]; !ok {
					if err := s.RemoveAll(path.Join(targetPath, baseName)); err != nil {
						return err
					}
				}
			}
		}
	}
	// FIXME chezmoiremove
	return nil
}

// ExecuteTemplateData returns the result of executing template data.
func (ss *SourceState) ExecuteTemplateData(name string, data []byte) ([]byte, error) {
	tmpl, err := template.New(name).Option(ss.templateOptions...).Funcs(ss.templateFuncs).Parse(string(data))
	if err != nil {
		return nil, err
	}
	for name, t := range ss.templates {
		tmpl, err = tmpl.AddParseTree(name, t.Tree)
		if err != nil {
			return nil, err
		}
	}
	output := &bytes.Buffer{}
	if err = tmpl.ExecuteTemplate(output, name, ss.templateData); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// Read reads a source state from sourcePath in fs.
func (ss *SourceState) Read() error {
	// Read all source entries.
	allEntries := make(map[string][]SourceStateEntry)
	sourceDirPrefix := filepath.ToSlash(ss.sourcePath) + pathSeparator
	if err := vfs.Walk(ss.s, ss.sourcePath, func(sourcePath string, info os.FileInfo, err error) error {
		sourcePath = filepath.ToSlash(sourcePath)
		if err != nil {
			return err
		}
		if sourcePath == ss.sourcePath {
			return nil
		}
		relPath := strings.TrimPrefix(sourcePath, sourceDirPrefix)
		dir, sourceName := path.Split(relPath)
		targetDirName := getTargetDirName(dir)
		switch {
		case info.Name() == ignoreName:
			return ss.addPatterns(ss.ignore, sourcePath, dir)
		case info.Name() == removeName:
			return ss.addPatterns(ss.remove, sourcePath, targetDirName)
		case info.Name() == templatesDirName:
			if err := ss.addTemplatesDir(sourcePath); err != nil {
				return err
			}
			return filepath.SkipDir
		case info.Name() == versionName:
			return ss.addVersionFile(sourcePath)
		case strings.HasPrefix(info.Name(), ignorePrefix):
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		case info.IsDir():
			dirAttributes := ParseDirAttributes(sourceName)
			targetName := path.Join(targetDirName, dirAttributes.Name)
			if ss.ignore.Match(targetName) {
				return nil
			}
			allEntries[targetName] = append(allEntries[targetName], ss.newSourceStateDir(sourcePath, dirAttributes))
			return nil
		case info.Mode().IsRegular():
			fileAttributes := ParseFileAttributes(sourceName)
			targetName := path.Join(targetDirName, fileAttributes.Name)
			if ss.ignore.Match(targetName) {
				return nil
			}
			allEntries[targetName] = append(allEntries[targetName], ss.newSourceStateFile(sourcePath, fileAttributes))
			return nil
		default:
			return &unsupportedFileTypeError{
				path: sourcePath,
				mode: info.Mode(),
			}
		}
	}); err != nil {
		return err
	}

	// Checking for duplicate source entries with the same target name. Iterate
	// over the target names in order so the error is deterministic.
	var err error
	targetNames := make([]string, 0, len(allEntries))
	for targetName := range allEntries {
		targetNames = append(targetNames, targetName)
	}
	sort.Strings(targetNames)
	for _, targetName := range targetNames {
		entries := allEntries[targetName]
		if len(entries) == 1 {
			continue
		}
		sourcePaths := make([]string, 0, len(entries))
		for _, sourceEntry := range entries {
			sourcePaths = append(sourcePaths, sourceEntry.Path())
		}
		err = multierr.Append(err, &duplicateTargetError{
			targetName:  targetName,
			sourcePaths: sourcePaths,
		})
	}
	if err != nil {
		return err
	}

	// Populate ss.entries.
	for targetName, sourceEntries := range allEntries {
		ss.entries[targetName] = sourceEntries[0]
	}
	return nil
}

// Remove removes everything in targetDir that matches s's remove pattern set.
func (ss *SourceState) Remove(s System, targetDir string) error {
	// Build a set of targets to remove.
	targetDirPrefix := targetDir + pathSeparator
	targetPathsToRemove := NewStringSet()
	for include := range ss.remove.includes {
		matches, err := s.Glob(path.Join(targetDir, include))
		if err != nil {
			return err
		}
		for _, match := range matches {
			// Don't remove targets that are excluded from remove.
			if !ss.remove.Match(strings.TrimPrefix(match, targetDirPrefix)) {
				continue
			}
			targetPathsToRemove.Add(match)
		}
	}

	sortedTargetPathsToRemove := targetPathsToRemove.Elements()
	sort.Strings(sortedTargetPathsToRemove)
	for _, targetPath := range sortedTargetPathsToRemove {
		if err := s.RemoveAll(targetPath); err != nil {
			return err
		}
	}
	return nil
}

// Evaluate evaluates every target state entry in s.
func (ss *SourceState) Evaluate() error {
	for _, targetName := range ss.sortedTargetNames() {
		sourceStateEntry := ss.entries[targetName]
		if err := sourceStateEntry.Evaluate(); err != nil {
			return err
		}
		targetStateEntry, err := sourceStateEntry.TargetStateEntry()
		if err != nil {
			return err
		}
		if err := targetStateEntry.Evaluate(); err != nil {
			return err
		}
	}
	return nil
}

func (ss *SourceState) addPatterns(ps *PatternSet, path, relPath string) error {
	data, err := ss.executeTemplate(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(relPath)
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		text := scanner.Text()
		if index := strings.IndexRune(text, '#'); index != -1 {
			text = text[:index]
		}
		text = strings.TrimSpace(text)
		if text == "" {
			continue
		}
		include := true
		if strings.HasPrefix(text, "!") {
			include = false
			text = strings.TrimPrefix(text, "!")
		}
		pattern := filepath.Join(dir, text)
		if err := ps.Add(pattern, include); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (ss *SourceState) addTemplatesDir(templateDir string) error {
	templateDirPrefix := filepath.ToSlash(templateDir) + pathSeparator
	return vfs.Walk(ss.s, templateDir, func(templatePath string, info os.FileInfo, err error) error {
		templatePath = filepath.ToSlash(templatePath)
		if err != nil {
			return err
		}
		switch {
		case info.Mode().IsRegular():
			contents, err := ss.s.ReadFile(templatePath)
			if err != nil {
				return err
			}
			name := strings.TrimPrefix(templatePath, templateDirPrefix)
			tmpl, err := template.New(name).Parse(string(contents))
			if err != nil {
				return err
			}
			if ss.templates == nil {
				ss.templates = make(map[string]*template.Template)
			}
			ss.templates[name] = tmpl
			return nil
		case info.IsDir():
			return nil
		default:
			return &unsupportedFileTypeError{
				path: templatePath,
				mode: info.Mode(),
			}
		}
	})
}

func (ss *SourceState) addVersionFile(sourcePath string) error {
	data, err := ss.s.ReadFile(sourcePath)
	if err != nil {
		return err
	}
	version, err := semver.NewVersion(strings.TrimSpace(string(data)))
	if err != nil {
		return err
	}
	if ss.minVersion == nil || ss.minVersion.LessThan(*version) {
		ss.minVersion = version
	}
	return nil
}

func (ss *SourceState) executeTemplate(path string) ([]byte, error) {
	data, err := ss.s.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ss.ExecuteTemplateData(path, data)
}

func (ss *SourceState) newSourceStateDir(sourcePath string, dirAttributes DirAttributes) *SourceStateDir {
	perm := os.FileMode(0o777)
	if dirAttributes.Private {
		perm &^= 0o77
	}
	perm &^= ss.umask

	targetStateDir := &TargetStateDir{
		perm:  perm,
		exact: dirAttributes.Exact,
	}

	return &SourceStateDir{
		path:             sourcePath,
		attributes:       dirAttributes,
		targetStateEntry: targetStateDir,
	}
}

func (ss *SourceState) newSourceStateFile(sourcePath string, fileAttributes FileAttributes) *SourceStateFile {
	lazyContents := &lazyContents{
		contentsFunc: func() ([]byte, error) {
			return ss.s.ReadFile(sourcePath)
		},
	}

	var targetStateEntryFunc func() (TargetStateEntry, error)
	switch fileAttributes.Type {
	case SourceFileTypeFile:
		targetStateEntryFunc = func() (TargetStateEntry, error) {
			contents, err := lazyContents.Contents()
			if err != nil {
				return nil, err
			}
			if !fileAttributes.Empty && isEmpty(contents) {
				return &TargetStateAbsent{}, nil
			}
			perm := os.FileMode(0o666)
			if fileAttributes.Executable {
				perm |= 0o111
			}
			if fileAttributes.Private {
				perm &^= 0o77
			}
			perm &^= ss.umask
			return &TargetStateFile{
				lazyContents: newLazyContents(contents),
				perm:         perm,
			}, nil
		}
	case SourceFileTypeScript:
		targetStateEntryFunc = func() (TargetStateEntry, error) {
			contents, err := lazyContents.Contents()
			if err != nil {
				return nil, err
			}
			return &TargetStateScript{
				lazyContents: newLazyContents(contents),
				name:         fileAttributes.Name,
				once:         fileAttributes.Once,
			}, nil
		}
	case SourceFileTypeSymlink:
		targetStateEntryFunc = func() (TargetStateEntry, error) {
			linknameBytes, err := lazyContents.Contents()
			if err != nil {
				return nil, err
			}
			return &TargetStateSymlink{
				lazyLinkname: newLazyLinkname(string(linknameBytes)),
			}, nil
		}
	default:
		panic(fmt.Sprintf("unsupported type: %s", string(fileAttributes.Type)))
	}

	return &SourceStateFile{
		lazyContents:         lazyContents,
		path:                 sourcePath,
		attributes:           fileAttributes,
		targetStateEntryFunc: targetStateEntryFunc,
	}
}

func (ss *SourceState) sortedTargetNames() []string {
	targetNames := make([]string, 0, len(ss.entries))
	for targetName := range ss.entries {
		targetNames = append(targetNames, targetName)
	}
	sort.Strings(targetNames)
	return targetNames
}

func getTargetDirName(dir string) string {
	sourceNames := strings.Split(dir, pathSeparator)
	targetNames := make([]string, 0, len(sourceNames))
	for _, sourceName := range sourceNames {
		dirAttributes := ParseDirAttributes(sourceName)
		targetNames = append(targetNames, dirAttributes.Name)
	}
	return strings.Join(targetNames, pathSeparator)
}