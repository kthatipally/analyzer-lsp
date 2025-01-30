package builtin

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/antchfx/jsonquery"
	"github.com/antchfx/xmlquery"
	"github.com/antchfx/xpath"
	"github.com/dlclark/regexp2"
	"github.com/go-logr/logr"
	"github.com/konveyor/analyzer-lsp/lsp/protocol"
	"github.com/konveyor/analyzer-lsp/provider"
	"github.com/konveyor/analyzer-lsp/tracing"
	"go.lsp.dev/uri"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v2"
)

type builtinServiceClient struct {
	config provider.InitConfig
	tags   map[string]bool
	provider.UnimplementedDependenciesComponent
	log logr.Logger

	cacheMutex    sync.RWMutex
	locationCache map[string]float64
	includedPaths []string
}

type fileTemplateContext struct {
	Filepaths []string `json:"filepaths,omitempty"`
}

var _ provider.ServiceClient = &builtinServiceClient{}

func (p *builtinServiceClient) Stop() {}

func (p *builtinServiceClient) Evaluate(ctx context.Context, cap string, conditionInfo []byte) (provider.ProviderEvaluateResponse, error) {
	var cond builtinCondition
	err := yaml.Unmarshal(conditionInfo, &cond)
	if err != nil {
		return provider.ProviderEvaluateResponse{}, fmt.Errorf("unable to get query info: %v", err)
	}
	log := p.log.WithValues("ruleID", cond.ProviderContext.RuleID)
	log.V(5).Info("builtin condition context", "condition", cond, "provider context", cond.ProviderContext)
	response := provider.ProviderEvaluateResponse{Matched: false}
	switch cap {
	case "file":
		c := cond.File
		if c.Pattern == "" {
			return response, fmt.Errorf("could not parse provided file pattern as string: %v", conditionInfo)
		}
		matchingFiles := []string{}
		if ok, paths := cond.ProviderContext.GetScopedFilepaths(); ok {
			regex, _ := regexp.Compile(c.Pattern)
			for _, path := range paths {
				matched := false
				if regex != nil {
					matched = regex.MatchString(path)
				} else {
					// TODO(fabianvf): is a fileglob style pattern sufficient or do we need regexes?
					matched, err = filepath.Match(c.Pattern, path)
					if err != nil {
						continue
					}
				}
				if matched {
					matchingFiles = append(matchingFiles, path)
				}
			}
		} else {
			matchingFiles, err = findFilesMatchingPattern(p.config.Location, c.Pattern)
			if err != nil {
				return response, fmt.Errorf("unable to find files using pattern `%s`: %v", c.Pattern, err)
			}
		}

		response.TemplateContext = map[string]interface{}{"filepaths": matchingFiles}
		for _, match := range matchingFiles {
			absPath := match
			if !filepath.IsAbs(match) {
				absPath, err = filepath.Abs(match)
				if err != nil {
					p.log.V(5).Error(err, "failed to get absolute path to file", "path", match)
					absPath = match
				}
			}
			if !p.isFileIncluded(absPath) {
				continue
			}
			response.Incidents = append(response.Incidents, provider.IncidentContext{
				FileURI: uri.File(absPath),
			})
		}
		response.Matched = len(response.Incidents) > 0
		return response, nil
	case "filecontent":
		c := cond.Filecontent
		if c.Pattern == "" {
			return response, fmt.Errorf("could not parse provided regex pattern as string: %v", conditionInfo)
		}

		patternRegex, err := regexp2.Compile(c.Pattern, regexp2.None)
		if err != nil {
			return response, fmt.Errorf("could not compile provided regex pattern '%s': %v", c.Pattern, err)
		}

		matches, err := parallelWalk(p.config.Location, patternRegex)
		if err != nil {
			return response, err
		}

		for _, match := range matches {
			containsFile, err := provider.FilterFilePattern(c.FilePattern, match.positionParams.TextDocument.URI)
			if err != nil {
				return response, err
			}
			if !containsFile {
				continue
			}
			lineNumber := int(match.positionParams.Position.Line)

			response.Incidents = append(response.Incidents, provider.IncidentContext{
				FileURI:    uri.URI(match.positionParams.TextDocument.URI),
				LineNumber: &lineNumber,
				Variables: map[string]interface{}{
					"matchingText": match.match,
				},
				CodeLocation: &provider.Location{
					StartPosition: provider.Position{Line: float64(lineNumber)},
					EndPosition:   provider.Position{Line: float64(lineNumber)},
				},
			})
		}
		if len(response.Incidents) != 0 {
			response.Matched = true
		}
		return response, nil
	case "xml":
		query, err := xpath.CompileWithNS(cond.XML.XPath, cond.XML.Namespaces)
		if query == nil || err != nil {
			return response, fmt.Errorf("could not parse provided xpath query '%s': %v", cond.XML.XPath, err)
		}
		filePaths := []string{}
		if ok, paths := cond.ProviderContext.GetScopedFilepaths(); ok {
			if len(cond.XML.Filepaths) > 0 {
				newPaths := []string{}
				// Sometimes rules have hardcoded filepaths
				// Or use other searching to get them. If so, then we
				// Should respect that added filter on the scoped filepaths
				for _, p := range cond.XML.Filepaths {
					for _, path := range paths {
						if p == path {
							newPaths = append(newPaths, path)
						}
						if filepath.Base(path) == p {
							newPaths = append(newPaths, path)
						}
					}
				}
				if len(newPaths) == 0 {
					// There are no files to search, return.
					return response, nil
				}
				filePaths = newPaths
			} else {
				filePaths = paths
			}
		} else if len(cond.XML.Filepaths) > 0 {
			filePaths = cond.XML.Filepaths
		}
		xmlFiles, err := findXMLFiles(p.config.Location, filePaths, log)
		if err != nil {
			return response, fmt.Errorf("unable to find XML files: %v", err)
		}
		for _, file := range xmlFiles {
			nodes, err := queryXMLFile(file, query)
			if err != nil {
				log.V(5).Error(err, "failed to query xml file", "file", file)
				continue
			}
			if len(nodes) != 0 {
				response.Matched = true
				for _, node := range nodes {
					absPath, err := filepath.Abs(file)
					if err != nil {
						absPath = file
					}
					if !p.isFileIncluded(absPath) {
						continue
					}
					incident := provider.IncidentContext{
						FileURI: uri.File(absPath),
						Variables: map[string]interface{}{
							"matchingXML": node.OutputXML(false),
							"innerText":   node.InnerText(),
							"data":        node.Data,
						},
					}
					content := strings.TrimSpace(node.InnerText())
					if content == "" {
						content = node.Data
					}
					location, err := p.getLocation(ctx, absPath, content)
					if err == nil {
						incident.CodeLocation = &location
						lineNo := int(location.StartPosition.Line)
						incident.LineNumber = &lineNo
					}
					response.Incidents = append(response.Incidents, incident)
				}
			}
		}

		return response, nil
	case "xmlPublicID":
		regex, err := regexp.Compile(cond.XMLPublicID.Regex)
		if err != nil {
			return response, fmt.Errorf("could not parse provided public-id regex '%s': %v", cond.XMLPublicID.Regex, err)
		}
		query, err := xpath.CompileWithNS("//*[@public-id]", cond.XMLPublicID.Namespaces)
		if query == nil || err != nil {
			return response, fmt.Errorf("could not parse public-id xml query '%s': %v", cond.XML.XPath, err)
		}
		filePaths := []string{}
		if ok, paths := cond.ProviderContext.GetScopedFilepaths(); ok {
			if len(cond.XML.Filepaths) > 0 {
				newPaths := []string{}
				// Sometimes rules have hardcoded filepaths
				// Or use other searching to get them. If so, then we
				// Should respect that added filter on the scoped filepaths
				for _, p := range cond.XML.Filepaths {
					for _, path := range paths {
						if p == path {
							newPaths = append(newPaths, path)
						}
					}
				}
				if len(newPaths) == 0 {
					// There are no files to search, return.
					return response, nil
				}
				filePaths = newPaths
			} else {
				filePaths = paths
			}
		} else if len(cond.XML.Filepaths) > 0 {
			filePaths = cond.XML.Filepaths
		}
		xmlFiles, err := findXMLFiles(p.config.Location, filePaths, p.log)
		if err != nil {
			return response, fmt.Errorf("unable to find XML files: %v", err)
		}
		for _, file := range xmlFiles {
			nodes, err := queryXMLFile(file, query)
			if err != nil {
				log.Error(err, "Keerthi failed to query xml file", "file", file)
				continue
			}

			for _, node := range nodes {
				// public-id attribute regex match check
				for _, attr := range node.Attr {
					if attr.Name.Local == "public-id" {
						if regex.MatchString(attr.Value) {
							response.Matched = true
							absPath, err := filepath.Abs(file)
							if err != nil {
								absPath = file
							}
							if !p.isFileIncluded(absPath) {
								continue
							}
							response.Incidents = append(response.Incidents, provider.IncidentContext{
								FileURI: uri.File(absPath),
								Variables: map[string]interface{}{
									"matchingXML": node.OutputXML(false),
									"innerText":   node.InnerText(),
									"data":        node.Data,
								},
							})
						}
						break
					}
				}
			}
		}

		return response, nil
	case "json":
		query := cond.JSON.XPath
		if query == "" {
			return response, fmt.Errorf("could not parse provided xpath query as string: %v", conditionInfo)
		}
		pattern := "*.json"
		filePaths := []string{}
		if ok, paths := cond.ProviderContext.GetScopedFilepaths(); ok {
			filePaths = paths
		} else if len(cond.XML.Filepaths) > 0 {
			filePaths = cond.JSON.Filepaths
		}
		jsonFiles, err := provider.GetFiles(p.config.Location, filePaths, pattern)
		if err != nil {
			return response, fmt.Errorf("unable to find files using pattern `%s`: %v", pattern, err)
		}
		for _, file := range jsonFiles {
			f, err := os.Open(file)
			if err != nil {
				log.V(5).Error(err, "error opening json file", "file", file)
				continue
			}
			doc, err := jsonquery.Parse(f)
			if err != nil {
				log.V(5).Error(err, "error parsing json file", "file", file)
				continue
			}
			list, err := jsonquery.QueryAll(doc, query)
			if err != nil {
				return response, err
			}
			if len(list) != 0 {
				response.Matched = true
				for _, node := range list {
					absPath, err := filepath.Abs(file)
					if err != nil {
						absPath = file
					}
					if !p.isFileIncluded(absPath) {
						continue
					}
					incident := provider.IncidentContext{
						FileURI: uri.File(absPath),
						Variables: map[string]interface{}{
							"matchingJSON": node.InnerText(),
							"data":         node.Data,
						},
					}
					location, err := p.getLocation(ctx, absPath, node.InnerText())
					if err == nil {
						incident.CodeLocation = &location
						lineNo := int(location.StartPosition.Line)
						incident.LineNumber = &lineNo
					}
					response.Incidents = append(response.Incidents, incident)
				}
			}
		}
		return response, nil
	case "hasTags":
		found := true
		for _, tag := range cond.HasTags {
			if _, exists := cond.ProviderContext.Tags[tag]; !exists {
				if _, exists := p.tags[tag]; !exists {
					found = false
					break
				}
			}
		}
		if found {
			response.Matched = true
			response.Incidents = append(response.Incidents, provider.IncidentContext{
				Variables: map[string]interface{}{
					"tags": cond.HasTags,
				},
			})
		}
		return response, nil
	default:
		return response, fmt.Errorf("capability must be one of %v, not %s", capabilities, cap)
	}
}

// getLocation attempts to get code location for given content in JSON / XML files
func (b *builtinServiceClient) getLocation(ctx context.Context, path, content string) (provider.Location, error) {
	ctx, span := tracing.StartNewSpan(ctx, "getLocation")
	defer span.End()
	location := provider.Location{}

	parts := strings.Split(content, "\n")
	if len(parts) < 1 {
		return location, fmt.Errorf("unable to get code location, empty content")
	} else if len(parts) > 5 {
		// limit content to search
		parts = parts[:5]
	}
	lines := []string{}
	for _, part := range parts {
		line := strings.Trim(part, " ")
		line = strings.ReplaceAll(line, "\t", "")
		line = regexp.QuoteMeta(line)
		lines = append(lines, line)
	}
	// remove leading and trailing empty lines
	if len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 1 || strings.Join(lines, "") == "" {
		return location, fmt.Errorf("unable to get code location, empty content")
	}
	pattern := fmt.Sprintf(".*?%s", strings.Join(lines, ".*?"))
	cacheKey := fmt.Sprintf("%s-%s", path, pattern)
	b.cacheMutex.RLock()
	val, exists := b.locationCache[cacheKey]
	b.cacheMutex.RUnlock()
	if exists {
		if val == -1 {
			return location, fmt.Errorf("unable to get location due to a previous error")
		}
		return provider.Location{
			StartPosition: provider.Position{
				Line: float64(val),
			},
			EndPosition: provider.Position{
				Line: float64(val),
			},
		}, nil
	}

	defer func() {
		b.cacheMutex.Lock()
		b.locationCache[cacheKey] = location.StartPosition.Line
		b.cacheMutex.Unlock()
	}()

	location.StartPosition.Line = -1
	lineNumber, err := provider.MultilineGrep(ctx, len(lines), path, pattern)
	if err != nil || lineNumber == -1 {
		return location, fmt.Errorf("unable to get location in file %s - %w", path, err)
	}
	location.StartPosition.Line = float64(lineNumber)
	location.EndPosition.Line = float64(lineNumber)
	return location, nil
}

func findFilesMatchingPattern(root, pattern string) ([]string, error) {
	var regex *regexp.Regexp
	// if the regex doesn't compile, we'll default to using filepath.Match on the pattern directly
	regex, _ = regexp.Compile(pattern)
	matches := []string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var matched bool
		if regex != nil {
			matched = regex.MatchString(d.Name())
		} else {
			// TODO(fabianvf): is a fileglob style pattern sufficient or do we need regexes?
			matched, err = filepath.Match(pattern, d.Name())
			if err != nil {
				return err
			}
		}
		if matched {
			matches = append(matches, path)
		}
		return nil
	})
	return matches, err
}

func findXMLFiles(baseLocation string, filePaths []string, log logr.Logger) ([]string, error) {
	patterns := []string{"*.xml", "*.xhtml"}
	// TODO(fabianvf): how should we scope the files searched here?
	xmlFiles, err := provider.GetFiles(baseLocation, filePaths, patterns...)
	return xmlFiles, err
}

func queryXMLFile(filePath string, query *xpath.Expr) (nodes []*xmlquery.Node, err error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("unable to open file '%s': %w", filePath, err)
	}
	defer f.Close()
	// TODO This should start working if/when this merges and releases: https://github.com/golang/go/pull/56848
	var doc *xmlquery.Node
	doc, err = xmlquery.ParseWithOptions(f, xmlquery.ParserOptions{Decoder: &xmlquery.DecoderOptions{Strict: false}})
	if err != nil {
		if err.Error() == "xml: unsupported version \"1.1\"; only version 1.0 is supported" {
			// TODO HACK just pretend 1.1 xml documents are 1.0 for now while we wait for golang to support 1.1
			var b []byte
			b, err = os.ReadFile(filePath)
			if err != nil {
				return nil, fmt.Errorf("unable to parse xml file '%s': %w", filePath, err)
			}
			docString := strings.Replace(string(b), "<?xml version=\"1.1\"", "<?xml version = \"1.0\"", 1)
			doc, err = xmlquery.Parse(strings.NewReader(docString))
			if err != nil {
				return nil, fmt.Errorf("unable to parse xml file '%s': %w", filePath, err)
			}
		} else {
			return nil, fmt.Errorf("unable to parse xml file '%s': %w", filePath, err)
		}
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("recovered panic from xpath query search with err - %v", r)
		}
	}()
	nodes = xmlquery.QuerySelectorAll(doc, query)
	return nodes, err
}

// filterByIncludedPaths given a list of file paths,
// filters-out the ones not present in includedPaths
func (b *builtinServiceClient) isFileIncluded(absolutePath string) bool {
	if b.includedPaths == nil || len(b.includedPaths) == 0 {
		return true
	}

	getSegments := func(path string) []string {
		segments := []string{}
		path = filepath.Clean(path)
		for _, segment := range strings.Split(
			path, string(os.PathSeparator)) {
			if segment != "" {
				segments = append(segments, segment)
			}
		}
		return segments
	}

	for _, path := range b.includedPaths {
		includedPath := filepath.Join(b.config.Location, path)
		if absPath, err := filepath.Abs(includedPath); err == nil {
			includedPath = absPath
		}
		pathSegments := getSegments(absolutePath)
		if stat, err := os.Stat(includedPath); err == nil && stat.IsDir() {
			pathSegments = getSegments(filepath.Dir(absolutePath))
		}
		includedPathSegments := getSegments(includedPath)
		if len(pathSegments) >= len(includedPathSegments) &&
			strings.HasPrefix(strings.Join(pathSegments, ""),
				strings.Join(includedPathSegments, "")) {
			return true
		}
	}
	b.log.V(7).Info("excluding file from search", "file", absolutePath)
	return false
}

type walkResult struct {
	positionParams protocol.TextDocumentPositionParams
	match          string
}

func parallelWalk(location string, regex *regexp2.Regexp) ([]walkResult, error) {

	var positions []walkResult
	var positionsMu sync.Mutex
	var eg errgroup.Group

	// Set a parallelism limit to avoid hitting limits related to opening too many files.
	// On Windows, this can show up as a runtime failure due to a thread limit.
	eg.SetLimit(256)

	err := filepath.Walk(location, func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if f.Mode().IsRegular() {
			eg.Go(func() error {
				pos, err := processFile(path, regex)
				if err != nil {
					return err
				}

				positionsMu.Lock()
				defer positionsMu.Unlock()
				positions = append(positions, pos...)
				return nil
			})
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := eg.Wait(); err != nil {
		return nil, err
	}

	return positions, nil
}

func processFile(path string, regex *regexp2.Regexp) ([]walkResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r []walkResult

	scanner := bufio.NewScanner(f)
	lineNumber := 1
	for scanner.Scan() {
		line := scanner.Text()
		match, err := regex.FindStringMatch(line)
		if err != nil {
			return nil, err
		}
		for match != nil {
			absPath, err := filepath.Abs(path)
			if err != nil {
				return nil, err
			}

			r = append(r, walkResult{
				positionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{
						URI: fmt.Sprintf("file:///%s", filepath.ToSlash(absPath)),
					},
					Position: protocol.Position{
						Line:      uint32(lineNumber),
						Character: uint32(match.Index),
					},
				},
				match: match.String(),
			})
			match, err = regex.FindNextMatch(match)
			if err != nil {
				return nil, err
			}
		}
		lineNumber++
	}

	return r, nil
}
