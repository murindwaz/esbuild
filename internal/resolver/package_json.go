package resolver

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/evanw/esbuild/internal/config"
	"github.com/evanw/esbuild/internal/js_ast"
	"github.com/evanw/esbuild/internal/js_lexer"
	"github.com/evanw/esbuild/internal/js_parser"
	"github.com/evanw/esbuild/internal/logger"
)

type packageJSON struct {
	source     logger.Source
	mainFields map[string]mainField
	moduleType config.ModuleType

	// Present if the "browser" field is present. This field is intended to be
	// used by bundlers and lets you redirect the paths of certain 3rd-party
	// modules that don't work in the browser to other modules that shim that
	// functionality. That way you don't have to rewrite the code for those 3rd-
	// party modules. For example, you might remap the native "util" node module
	// to something like https://www.npmjs.com/package/util so it works in the
	// browser.
	//
	// This field contains the original mapping object in "package.json". Mapping
	// to a nil path indicates that the module is disabled. As far as I can
	// tell, the official spec is an abandoned GitHub repo hosted by a user account:
	// https://github.com/defunctzombie/package-browser-field-spec. The npm docs
	// say almost nothing: https://docs.npmjs.com/files/package.json.
	//
	// Note that the non-package "browser" map has to be checked twice to match
	// Webpack's behavior: once before resolution and once after resolution. It
	// leads to some unintuitive failure cases that we must emulate around missing
	// file extensions:
	//
	// * Given the mapping "./no-ext": "./no-ext-browser.js" the query "./no-ext"
	//   should match but the query "./no-ext.js" should NOT match.
	//
	// * Given the mapping "./ext.js": "./ext-browser.js" the query "./ext.js"
	//   should match and the query "./ext" should ALSO match.
	//
	browserMap map[string]*string

	// If this is non-nil, each entry in this map is the absolute path of a file
	// with side effects. Any entry not in this map should be considered to have
	// no side effects, which means import statements for these files can be
	// removed if none of the imports are used. This is a convention from Webpack:
	// https://webpack.js.org/guides/tree-shaking/.
	//
	// Note that if a file is included, all statements that can't be proven to be
	// free of side effects must be included. This convention does not say
	// anything about whether any statements within the file have side effects or
	// not.
	sideEffectsMap     map[string]bool
	sideEffectsRegexps []*regexp.Regexp
	sideEffectsData    *SideEffectsData

	// This represents the "imports" field in this package.json file.
	importsMap *pjMap

	// This represents the "exports" field in this package.json file.
	exportsMap *pjMap
}

type mainField struct {
	keyLoc  logger.Loc
	relPath string
}

type browserPathKind uint8

const (
	absolutePathKind browserPathKind = iota
	packagePathKind
)

func (r resolverQuery) checkBrowserMap(resolveDirInfo *dirInfo, inputPath string, kind browserPathKind) (remapped *string, ok bool) {
	// This only applies if the current platform is "browser"
	if r.options.Platform != config.PlatformBrowser {
		return nil, false
	}

	// There must be an enclosing directory with a "package.json" file with a "browser" map
	if resolveDirInfo.enclosingBrowserScope == nil {
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("No \"browser\" map found in directory %q", resolveDirInfo.absPath))
		}
		return nil, false
	}

	packageJSON := resolveDirInfo.enclosingBrowserScope.packageJSON
	browserMap := packageJSON.browserMap

	checkPath := func(pathToCheck string) bool {
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("Checking for %q in the \"browser\" map in %q",
				pathToCheck, packageJSON.source.KeyPath.Text))
		}

		// Check for equality
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("  Checking for %q", pathToCheck))
		}
		remapped, ok = browserMap[pathToCheck]
		if ok {
			inputPath = pathToCheck
			return true
		}

		// If that failed, try adding implicit extensions
		for _, ext := range r.options.ExtensionOrder {
			extPath := pathToCheck + ext
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("  Checking for %q", extPath))
			}
			remapped, ok = browserMap[extPath]
			if ok {
				inputPath = extPath
				return true
			}
		}

		// If that failed, try assuming this is a directory and looking for an "index" file
		indexPath := path.Join(pathToCheck, "index")
		if IsPackagePath(indexPath) && !IsPackagePath(pathToCheck) {
			indexPath = "./" + indexPath
		}

		// Check for equality
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("  Checking for %q", indexPath))
		}
		remapped, ok = browserMap[indexPath]
		if ok {
			inputPath = indexPath
			return true
		}

		// If that failed, try adding implicit extensions
		for _, ext := range r.options.ExtensionOrder {
			extPath := indexPath + ext
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("  Checking for %q", extPath))
			}
			remapped, ok = browserMap[extPath]
			if ok {
				inputPath = extPath
				return true
			}
		}

		return false
	}

	// Turn absolute paths into paths relative to the "browser" map location
	if kind == absolutePathKind {
		relPath, ok := r.fs.Rel(resolveDirInfo.enclosingBrowserScope.absPath, inputPath)
		if !ok {
			return nil, false
		}
		inputPath = strings.ReplaceAll(relPath, "\\", "/")
	}

	if inputPath == "." {
		// No bundler supports remapping ".", so we don't either
		return nil, false
	}

	// First try the import path as a package path
	if !checkPath(inputPath) && IsPackagePath(inputPath) {
		// If a package path didn't work, try the import path as a relative path
		switch kind {
		case absolutePathKind:
			checkPath("./" + inputPath)

		case packagePathKind:
			// Browserify allows a browser map entry of "./pkg" to override a package
			// path of "require('pkg')". This is weird, and arguably a bug. But we
			// replicate this bug for compatibility. However, Browserify only allows
			// this within the same package. It does not allow such an entry in a
			// parent package to override this in a child package. So this behavior
			// is disallowed if there is a "node_modules" folder in between the child
			// package and the parent package.
			isInSamePackage := true
			for info := resolveDirInfo; info != nil && info != resolveDirInfo.enclosingBrowserScope; info = info.parent {
				if info.isNodeModules {
					isInSamePackage = false
					break
				}
			}
			if isInSamePackage {
				checkPath("./" + inputPath)
			}
		}
	}

	if r.debugLogs != nil {
		if ok {
			if remapped == nil {
				r.debugLogs.addNote(fmt.Sprintf("Found %q marked as disabled", inputPath))
			} else {
				r.debugLogs.addNote(fmt.Sprintf("Found %q mapping to %q", inputPath, *remapped))
			}
		} else {
			r.debugLogs.addNote(fmt.Sprintf("Failed to find %q", inputPath))
		}
	}
	return
}

func (r resolverQuery) parsePackageJSON(inputPath string) *packageJSON {
	packageJSONPath := r.fs.Join(inputPath, "package.json")
	contents, err, originalError := r.caches.FSCache.ReadFile(r.fs, packageJSONPath)
	if r.debugLogs != nil && originalError != nil {
		r.debugLogs.addNote(fmt.Sprintf("Failed to read file %q: %s", packageJSONPath, originalError.Error()))
	}
	if err != nil {
		r.log.Add(logger.Error, nil, logger.Range{},
			fmt.Sprintf("Cannot read file %q: %s",
				r.PrettyPath(logger.Path{Text: packageJSONPath, Namespace: "file"}), err.Error()))
		return nil
	}
	if r.debugLogs != nil {
		r.debugLogs.addNote(fmt.Sprintf("The file %q exists", packageJSONPath))
	}

	keyPath := logger.Path{Text: packageJSONPath, Namespace: "file"}
	jsonSource := logger.Source{
		KeyPath:    keyPath,
		PrettyPath: r.PrettyPath(keyPath),
		Contents:   contents,
	}
	tracker := logger.MakeLineColumnTracker(&jsonSource)

	json, ok := r.caches.JSONCache.Parse(r.log, jsonSource, js_parser.JSONOptions{})
	if !ok {
		return nil
	}

	packageJSON := &packageJSON{
		source:     jsonSource,
		mainFields: make(map[string]mainField),
	}

	// Read the "type" field
	if typeJSON, _, ok := getProperty(json, "type"); ok {
		if typeValue, ok := getString(typeJSON); ok {
			switch typeValue {
			case "commonjs":
				packageJSON.moduleType = config.ModuleCommonJS
			case "module":
				packageJSON.moduleType = config.ModuleESM
			default:
				r.log.AddWithNotes(logger.Warning, &tracker, jsonSource.RangeOfString(typeJSON.Loc),
					fmt.Sprintf("%q is not a valid value for the \"type\" field", typeValue),
					[]logger.MsgData{{Text: "The \"type\" field must be set to either \"commonjs\" or \"module\"."}},
				)
			}
		} else {
			r.log.Add(logger.Warning, &tracker, logger.Range{Loc: typeJSON.Loc},
				"The value for \"type\" must be a string")
		}
	}

	// Read the "main" fields
	mainFields := r.options.MainFields
	if mainFields == nil {
		mainFields = defaultMainFields[r.options.Platform]
	}
	for _, field := range mainFields {
		if mainJSON, mainRange, ok := getProperty(json, field); ok {
			if main, ok := getString(mainJSON); ok && main != "" {
				packageJSON.mainFields[field] = mainField{mainRange, main}
			}
		}
	}
	for _, field := range mainFieldsForFailure {
		if _, ok := packageJSON.mainFields[field]; !ok {
			if mainJSON, mainRange, ok := getProperty(json, field); ok {
				if main, ok := getString(mainJSON); ok && main != "" {
					packageJSON.mainFields[field] = mainField{mainRange, main}
				}
			}
		}
	}

	// Read the "browser" property, but only when targeting the browser
	if browserJSON, _, ok := getProperty(json, "browser"); ok && r.options.Platform == config.PlatformBrowser {
		// We both want the ability to have the option of CJS vs. ESM and the
		// option of having node vs. browser. The way to do this is to use the
		// object literal form of the "browser" field like this:
		//
		//   "main": "dist/index.node.cjs.js",
		//   "module": "dist/index.node.esm.js",
		//   "browser": {
		//     "./dist/index.node.cjs.js": "./dist/index.browser.cjs.js",
		//     "./dist/index.node.esm.js": "./dist/index.browser.esm.js"
		//   },
		//
		if browser, ok := browserJSON.Data.(*js_ast.EObject); ok {
			// The value is an object
			browserMap := make(map[string]*string)

			// Remap all files in the browser field
			for _, prop := range browser.Properties {
				if key, ok := getString(prop.Key); ok && prop.ValueOrNil.Data != nil {
					if value, ok := getString(prop.ValueOrNil); ok {
						// If this is a string, it's a replacement package
						browserMap[key] = &value
					} else if value, ok := getBool(prop.ValueOrNil); ok {
						// If this is false, it means the package is disabled
						if !value {
							browserMap[key] = nil
						}
					} else {
						r.log.Add(logger.Warning, &tracker, logger.Range{Loc: prop.ValueOrNil.Loc},
							"Each \"browser\" mapping must be a string or a boolean")
					}
				}
			}

			packageJSON.browserMap = browserMap
		}
	}

	// Read the "sideEffects" property
	if sideEffectsJSON, sideEffectsLoc, ok := getProperty(json, "sideEffects"); ok {
		switch data := sideEffectsJSON.Data.(type) {
		case *js_ast.EBoolean:
			if !data.Value {
				// Make an empty map for "sideEffects: false", which indicates all
				// files in this module can be considered to not have side effects.
				packageJSON.sideEffectsMap = make(map[string]bool)
				packageJSON.sideEffectsData = &SideEffectsData{
					IsSideEffectsArrayInJSON: false,
					Source:                   &jsonSource,
					Range:                    jsonSource.RangeOfString(sideEffectsLoc),
				}
			}

		case *js_ast.EArray:
			// The "sideEffects: []" format means all files in this module but not in
			// the array can be considered to not have side effects.
			packageJSON.sideEffectsMap = make(map[string]bool)
			packageJSON.sideEffectsData = &SideEffectsData{
				IsSideEffectsArrayInJSON: true,
				Source:                   &jsonSource,
				Range:                    jsonSource.RangeOfString(sideEffectsLoc),
			}
			for _, itemJSON := range data.Items {
				item, ok := itemJSON.Data.(*js_ast.EString)
				if !ok || item.Value == nil {
					r.log.Add(logger.Warning, &tracker, logger.Range{Loc: itemJSON.Loc},
						"Expected string in array for \"sideEffects\"")
					continue
				}

				// Reference: https://github.com/webpack/webpack/blob/ed175cd22f89eb9fecd0a70572a3fd0be028e77c/lib/optimize/SideEffectsFlagPlugin.js
				pattern := js_lexer.UTF16ToString(item.Value)
				if !strings.ContainsRune(pattern, '/') {
					pattern = "**/" + pattern
				}
				absPattern := r.fs.Join(inputPath, pattern)
				re, hadWildcard := globstarToEscapedRegexp(absPattern)

				// Wildcard patterns require more expensive matching
				if hadWildcard {
					packageJSON.sideEffectsRegexps = append(packageJSON.sideEffectsRegexps, regexp.MustCompile(re))
					continue
				}

				// Normal strings can be matched with a map lookup
				packageJSON.sideEffectsMap[absPattern] = true
			}

		default:
			r.log.Add(logger.Warning, &tracker, logger.Range{Loc: sideEffectsJSON.Loc},
				"The value for \"sideEffects\" must be a boolean or an array")
		}
	}

	// Read the "imports" map
	if importsJSON, _, ok := getProperty(json, "imports"); ok {
		if importsMap := parseImportsExportsMap(jsonSource, r.log, importsJSON); importsMap != nil {
			if importsMap.root.kind != pjObject {
				r.log.Add(logger.Warning, &tracker, importsMap.root.firstToken,
					"The value for \"imports\" must be an object")
			}
			packageJSON.importsMap = importsMap
		}
	}

	// Read the "exports" map
	if exportsJSON, _, ok := getProperty(json, "exports"); ok {
		if exportsMap := parseImportsExportsMap(jsonSource, r.log, exportsJSON); exportsMap != nil {
			packageJSON.exportsMap = exportsMap
		}
	}

	return packageJSON
}

// Reference: https://github.com/fitzgen/glob-to-regexp/blob/2abf65a834259c6504ed3b80e85f893f8cd99127/index.js
func globstarToEscapedRegexp(glob string) (string, bool) {
	sb := strings.Builder{}
	sb.WriteByte('^')
	hadWildcard := false
	n := len(glob)

	for i := 0; i < n; i++ {
		c := glob[i]
		switch c {
		case '\\', '^', '$', '.', '+', '|', '(', ')', '[', ']', '{', '}':
			sb.WriteByte('\\')
			sb.WriteByte(c)

		case '?':
			sb.WriteByte('.')
			hadWildcard = true

		case '*':
			// Move over all consecutive "*"'s.
			// Also store the previous and next characters
			prevChar := -1
			if i > 0 {
				prevChar = int(glob[i-1])
			}
			starCount := 1
			for i+1 < n && glob[i+1] == '*' {
				starCount++
				i++
			}
			nextChar := -1
			if i+1 < n {
				nextChar = int(glob[i+1])
			}

			// Determine if this is a globstar segment
			isGlobstar := starCount > 1 && // multiple "*"'s
				(prevChar == '/' || prevChar == -1) && // from the start of the segment
				(nextChar == '/' || nextChar == -1) // to the end of the segment

			if isGlobstar {
				// It's a globstar, so match zero or more path segments
				sb.WriteString("(?:[^/]*(?:/|$))*")
				i++ // Move over the "/"
			} else {
				// It's not a globstar, so only match one path segment
				sb.WriteString("[^/]*")
			}

			hadWildcard = true

		default:
			sb.WriteByte(c)
		}
	}

	sb.WriteByte('$')
	return sb.String(), hadWildcard
}

// Reference: https://nodejs.org/api/esm.html#esm_resolver_algorithm_specification
type pjMap struct {
	root pjEntry
}

type pjKind uint8

const (
	pjNull pjKind = iota
	pjString
	pjArray
	pjObject
	pjInvalid
)

type pjEntry struct {
	strData       string
	arrData       []pjEntry
	mapData       []pjMapEntry // Can't be a "map" because order matters
	expansionKeys expansionKeysArray
	firstToken    logger.Range
	kind          pjKind
}

type pjMapEntry struct {
	key      string
	keyRange logger.Range
	value    pjEntry
}

// This type is just so we can use Go's native sort function
type expansionKeysArray []pjMapEntry

func (a expansionKeysArray) Len() int          { return len(a) }
func (a expansionKeysArray) Swap(i int, j int) { a[i], a[j] = a[j], a[i] }

func (a expansionKeysArray) Less(i int, j int) bool {
	return len(a[i].key) > len(a[j].key)
}

func (entry pjEntry) valueForKey(key string) (pjEntry, bool) {
	for _, item := range entry.mapData {
		if item.key == key {
			return item.value, true
		}
	}
	return pjEntry{}, false
}

func parseImportsExportsMap(source logger.Source, log logger.Log, json js_ast.Expr) *pjMap {
	var visit func(expr js_ast.Expr) pjEntry
	tracker := logger.MakeLineColumnTracker(&source)

	visit = func(expr js_ast.Expr) pjEntry {
		var firstToken logger.Range

		switch e := expr.Data.(type) {
		case *js_ast.ENull:
			return pjEntry{
				kind:       pjNull,
				firstToken: js_lexer.RangeOfIdentifier(source, expr.Loc),
			}

		case *js_ast.EString:
			return pjEntry{
				kind:       pjString,
				firstToken: source.RangeOfString(expr.Loc),
				strData:    js_lexer.UTF16ToString(e.Value),
			}

		case *js_ast.EArray:
			arrData := make([]pjEntry, len(e.Items))
			for i, item := range e.Items {
				arrData[i] = visit(item)
			}
			return pjEntry{
				kind:       pjArray,
				firstToken: logger.Range{Loc: expr.Loc, Len: 1},
				arrData:    arrData,
			}

		case *js_ast.EObject:
			mapData := make([]pjMapEntry, len(e.Properties))
			expansionKeys := make(expansionKeysArray, 0, len(e.Properties))
			firstToken := logger.Range{Loc: expr.Loc, Len: 1}
			isConditionalSugar := false

			for i, property := range e.Properties {
				keyStr, _ := property.Key.Data.(*js_ast.EString)
				key := js_lexer.UTF16ToString(keyStr.Value)
				keyRange := source.RangeOfString(property.Key.Loc)

				// If exports is an Object with both a key starting with "." and a key
				// not starting with ".", throw an Invalid Package Configuration error.
				curIsConditionalSugar := !strings.HasPrefix(key, ".")
				if i == 0 {
					isConditionalSugar = curIsConditionalSugar
				} else if isConditionalSugar != curIsConditionalSugar {
					prevEntry := mapData[i-1]
					log.AddWithNotes(logger.Warning, &tracker, keyRange,
						"This object cannot contain keys that both start with \".\" and don't start with \".\"",
						[]logger.MsgData{tracker.MsgData(prevEntry.keyRange,
							fmt.Sprintf("The key %q is incompatible with the previous key %q:", key, prevEntry.key))})
					return pjEntry{
						kind:       pjInvalid,
						firstToken: firstToken,
					}
				}

				entry := pjMapEntry{
					key:      key,
					keyRange: keyRange,
					value:    visit(property.ValueOrNil),
				}

				if strings.HasSuffix(key, "/") || strings.HasSuffix(key, "*") {
					expansionKeys = append(expansionKeys, entry)
				}

				mapData[i] = entry
			}

			// Let expansionKeys be the list of keys of matchObj ending in "/" or "*",
			// sorted by length descending.
			sort.Stable(expansionKeys)

			return pjEntry{
				kind:          pjObject,
				firstToken:    firstToken,
				mapData:       mapData,
				expansionKeys: expansionKeys,
			}

		case *js_ast.EBoolean:
			firstToken = js_lexer.RangeOfIdentifier(source, expr.Loc)

		case *js_ast.ENumber:
			firstToken = source.RangeOfNumber(expr.Loc)

		default:
			firstToken.Loc = expr.Loc
		}

		log.Add(logger.Warning, &tracker, firstToken,
			"This value must be a string, an object, an array, or null")
		return pjEntry{
			kind:       pjInvalid,
			firstToken: firstToken,
		}
	}

	root := visit(json)

	if root.kind == pjNull {
		return nil
	}

	return &pjMap{root: root}
}

func (entry pjEntry) keysStartWithDot() bool {
	return len(entry.mapData) > 0 && strings.HasPrefix(entry.mapData[0].key, ".")
}

type pjStatus uint8

const (
	pjStatusUndefined                  pjStatus = iota
	pjStatusUndefinedNoConditionsMatch          // A more friendly error message for when no conditions are matched
	pjStatusNull
	pjStatusExact
	pjStatusInexact        // This means we may need to try CommonJS-style extension suffixes
	pjStatusPackageResolve // Need to re-run package resolution on the result

	// Module specifier is an invalid URL, package name or package subpath specifier.
	pjStatusInvalidModuleSpecifier

	// package.json configuration is invalid or contains an invalid configuration.
	pjStatusInvalidPackageConfiguration

	// Package exports or imports define a target module for the package that is an invalid type or string target.
	pjStatusInvalidPackageTarget

	// Package exports do not define or permit a target subpath in the package for the given module.
	pjStatusPackagePathNotExported

	// Package imports do not define the specifiespecifier
	pjStatusPackageImportNotDefined

	// The package or module requested does not exist.
	pjStatusModuleNotFound

	// The resolved path corresponds to a directory, which is not a supported target for module imports.
	pjStatusUnsupportedDirectoryImport
)

func (status pjStatus) isUndefined() bool {
	return status == pjStatusUndefined || status == pjStatusUndefinedNoConditionsMatch
}

type pjDebug struct {
	// This is the range of the token to use for error messages
	token logger.Range

	// If the status is "pjStatusUndefinedNoConditionsMatch", this is the set of
	// conditions that didn't match. This information is used for error messages.
	unmatchedConditions []string
}

func (r resolverQuery) esmHandlePostConditions(
	resolved string,
	status pjStatus,
	debug pjDebug,
) (string, pjStatus, pjDebug) {
	if status != pjStatusExact && status != pjStatusInexact {
		return resolved, status, debug
	}

	// If resolved contains any percent encodings of "/" or "\" ("%2f" and "%5C"
	// respectively), then throw an Invalid Module Specifier error.
	resolvedPath, err := url.PathUnescape(resolved)
	if err != nil {
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("The path %q contains invalid URL escapes: %s", resolved, err.Error()))
		}
		return resolved, pjStatusInvalidModuleSpecifier, debug
	}
	var found string
	if strings.Contains(resolved, "%2f") {
		found = "%2f"
	} else if strings.Contains(resolved, "%2F") {
		found = "%2F"
	} else if strings.Contains(resolved, "%5c") {
		found = "%5c"
	} else if strings.Contains(resolved, "%5C") {
		found = "%5C"
	}
	if found != "" {
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("The path %q is not allowed to contain %q", resolved, found))
		}
		return resolved, pjStatusInvalidModuleSpecifier, debug
	}

	// If the file at resolved is a directory, then throw an Unsupported Directory
	// Import error.
	if strings.HasSuffix(resolvedPath, "/") || strings.HasSuffix(resolvedPath, "\\") {
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("The path %q is not allowed to end with a slash", resolved))
		}
		return resolved, pjStatusUnsupportedDirectoryImport, debug
	}

	// Set resolved to the real path of resolved.
	return resolvedPath, status, debug
}

func (r resolverQuery) esmPackageImportsResolve(
	specifier string,
	imports pjEntry,
	conditions map[string]bool,
) (string, pjStatus, pjDebug) {
	// ALGORITHM DEVIATION: Provide a friendly error message if "imports" is not an object
	if imports.kind != pjObject {
		return "", pjStatusInvalidPackageConfiguration, pjDebug{token: imports.firstToken}
	}

	resolved, status, debug := r.esmPackageImportsExportsResolve(specifier, imports, "/", true, conditions)
	if status != pjStatusNull && status != pjStatusUndefined {
		return resolved, status, debug
	}

	if r.debugLogs != nil {
		r.debugLogs.addNote(fmt.Sprintf("The package import %q is not defined", specifier))
	}
	return specifier, pjStatusPackageImportNotDefined, pjDebug{token: imports.firstToken}
}

func (r resolverQuery) esmPackageExportsResolve(
	packageURL string,
	subpath string,
	exports pjEntry,
	conditions map[string]bool,
) (string, pjStatus, pjDebug) {
	if exports.kind == pjInvalid {
		if r.debugLogs != nil {
			r.debugLogs.addNote("Invalid package configuration")
		}
		return "", pjStatusInvalidPackageConfiguration, pjDebug{token: exports.firstToken}
	}

	if subpath == "." {
		mainExport := pjEntry{kind: pjNull}
		if exports.kind == pjString || exports.kind == pjArray || (exports.kind == pjObject && !exports.keysStartWithDot()) {
			mainExport = exports
		} else if exports.kind == pjObject {
			if dot, ok := exports.valueForKey("."); ok {
				if r.debugLogs != nil {
					r.debugLogs.addNote("Using the entry for \".\"")
				}
				mainExport = dot
			}
		}
		if mainExport.kind != pjNull {
			resolved, status, debug := r.esmPackageTargetResolve(packageURL, mainExport, "", false, false, conditions)
			if status != pjStatusNull && status != pjStatusUndefined {
				return resolved, status, debug
			}
		}
	} else if exports.kind == pjObject && exports.keysStartWithDot() {
		resolved, status, debug := r.esmPackageImportsExportsResolve(subpath, exports, packageURL, false, conditions)
		if status != pjStatusNull && status != pjStatusUndefined {
			return resolved, status, debug
		}
	}

	if r.debugLogs != nil {
		r.debugLogs.addNote(fmt.Sprintf("The path %q is not exported", subpath))
	}
	return "", pjStatusPackagePathNotExported, pjDebug{token: exports.firstToken}
}

func (r resolverQuery) esmPackageImportsExportsResolve(
	matchKey string,
	matchObj pjEntry,
	packageURL string,
	isImports bool,
	conditions map[string]bool,
) (string, pjStatus, pjDebug) {
	if r.debugLogs != nil {
		r.debugLogs.addNote(fmt.Sprintf("Checking object path map for %q", matchKey))
	}

	if !strings.HasSuffix(matchKey, "*") {
		if target, ok := matchObj.valueForKey(matchKey); ok {
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("Found exact match for %q", matchKey))
			}
			return r.esmPackageTargetResolve(packageURL, target, "", false, isImports, conditions)
		}
	}

	for _, expansion := range matchObj.expansionKeys {
		// If expansionKey ends in "*" and matchKey starts with but is not equal to
		// the substring of expansionKey excluding the last "*" character
		if strings.HasSuffix(expansion.key, "*") {
			if substr := expansion.key[:len(expansion.key)-1]; strings.HasPrefix(matchKey, substr) && matchKey != substr {
				target := expansion.value
				subpath := matchKey[len(expansion.key)-1:]
				if r.debugLogs != nil {
					r.debugLogs.addNote(fmt.Sprintf("The key %q matched with %q left over", expansion.key, subpath))
				}
				return r.esmPackageTargetResolve(packageURL, target, subpath, true, isImports, conditions)
			}
		}

		if strings.HasPrefix(matchKey, expansion.key) {
			target := expansion.value
			subpath := matchKey[len(expansion.key):]
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("The key %q matched with %q left over", expansion.key, subpath))
			}
			result, status, debug := r.esmPackageTargetResolve(packageURL, target, subpath, false, isImports, conditions)
			if status == pjStatusExact {
				// Return the object { resolved, exact: false }.
				status = pjStatusInexact
			}
			return result, status, debug
		}

		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("The key %q did not match", expansion.key))
		}
	}

	if r.debugLogs != nil {
		r.debugLogs.addNote(fmt.Sprintf("No keys matched %q", matchKey))
	}
	return "", pjStatusNull, pjDebug{token: matchObj.firstToken}
}

// If path split on "/" or "\" contains any ".", ".." or "node_modules"
// segments after the first segment, throw an Invalid Package Target error.
func findInvalidSegment(path string) string {
	slash := strings.IndexAny(path, "/\\")
	if slash == -1 {
		return ""
	}
	path = path[slash+1:]
	for path != "" {
		slash := strings.IndexAny(path, "/\\")
		segment := path
		if slash != -1 {
			segment = path[:slash]
			path = path[slash+1:]
		} else {
			path = ""
		}
		if segment == "." || segment == ".." || segment == "node_modules" {
			return segment
		}
	}
	return ""
}

func (r resolverQuery) esmPackageTargetResolve(
	packageURL string,
	target pjEntry,
	subpath string,
	pattern bool,
	internal bool,
	conditions map[string]bool,
) (string, pjStatus, pjDebug) {
	switch target.kind {
	case pjString:
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("Checking path %q against target %q", subpath, target.strData))
			r.debugLogs.increaseIndent()
			defer r.debugLogs.decreaseIndent()
		}

		// If pattern is false, subpath has non-zero length and target
		// does not end with "/", throw an Invalid Module Specifier error.
		if !pattern && subpath != "" && !strings.HasSuffix(target.strData, "/") {
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("The target %q is invalid because it doesn't end \"/\"", target.strData))
			}
			return target.strData, pjStatusInvalidModuleSpecifier, pjDebug{token: target.firstToken}
		}

		// If target does not start with "./", then...
		if !strings.HasPrefix(target.strData, "./") {
			if internal && !strings.HasPrefix(target.strData, "../") && !strings.HasPrefix(target.strData, "/") {
				if pattern {
					result := strings.ReplaceAll(target.strData, "*", subpath)
					if r.debugLogs != nil {
						r.debugLogs.addNote(fmt.Sprintf("Substituted %q for \"*\" in %q to get %q", subpath, target.strData, result))
					}
					return result, pjStatusPackageResolve, pjDebug{token: target.firstToken}
				}
				result := target.strData + subpath
				if r.debugLogs != nil {
					r.debugLogs.addNote(fmt.Sprintf("Joined %q to %q to get %q", target.strData, subpath, result))
				}
				return result, pjStatusPackageResolve, pjDebug{token: target.firstToken}
			}
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("The target %q is invalid because it doesn't start with \"./\"", target.strData))
			}
			return target.strData, pjStatusInvalidPackageTarget, pjDebug{token: target.firstToken}
		}

		// If target split on "/" or "\" contains any ".", ".." or "node_modules"
		// segments after the first segment, throw an Invalid Package Target error.
		if invalidSegment := findInvalidSegment(target.strData); invalidSegment != "" {
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("The target %q is invalid because it contains invalid segment %q", target.strData, invalidSegment))
			}
			return target.strData, pjStatusInvalidPackageTarget, pjDebug{token: target.firstToken}
		}

		// Let resolvedTarget be the URL resolution of the concatenation of packageURL and target.
		resolvedTarget := path.Join(packageURL, target.strData)

		// If subpath split on "/" or "\" contains any ".", ".." or "node_modules"
		// segments, throw an Invalid Module Specifier error.
		if invalidSegment := findInvalidSegment(subpath); invalidSegment != "" {
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("The path %q is invalid because it contains invalid segment %q", subpath, invalidSegment))
			}
			return subpath, pjStatusInvalidModuleSpecifier, pjDebug{token: target.firstToken}
		}

		if pattern {
			// Return the URL resolution of resolvedTarget with every instance of "*" replaced with subpath.
			result := strings.ReplaceAll(resolvedTarget, "*", subpath)
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("Substituted %q for \"*\" in %q to get %q", subpath, "."+resolvedTarget, "."+result))
			}
			return result, pjStatusExact, pjDebug{token: target.firstToken}
		} else {
			// Return the URL resolution of the concatenation of subpath and resolvedTarget.
			result := path.Join(resolvedTarget, subpath)
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("Joined %q to %q to get %q", subpath, "."+resolvedTarget, "."+result))
			}
			return result, pjStatusExact, pjDebug{token: target.firstToken}
		}

	case pjObject:
		if r.debugLogs != nil {
			keys := make([]string, 0, len(conditions))
			for key := range conditions {
				keys = append(keys, fmt.Sprintf("%q", key))
			}
			sort.Strings(keys)
			r.debugLogs.addNote(fmt.Sprintf("Checking condition map for one of [%s]", strings.Join(keys, ", ")))
			r.debugLogs.increaseIndent()
			defer r.debugLogs.decreaseIndent()
		}

		var didFindMapEntry bool
		var lastMapEntry pjMapEntry

		for _, p := range target.mapData {
			if p.key == "default" || conditions[p.key] {
				if r.debugLogs != nil {
					r.debugLogs.addNote(fmt.Sprintf("The key %q applies", p.key))
				}
				resolved, status, debug := r.esmPackageTargetResolve(packageURL, p.value, subpath, pattern, internal, conditions)
				if status.isUndefined() {
					didFindMapEntry = true
					lastMapEntry = p
					continue
				}
				return resolved, status, debug
			}
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("The key %q does not apply", p.key))
			}
		}

		if r.debugLogs != nil {
			r.debugLogs.addNote("No keys in the map were applicable")
		}

		// ALGORITHM DEVIATION: Provide a friendly error message if no conditions matched
		if len(target.mapData) > 0 && !target.keysStartWithDot() {
			if didFindMapEntry && lastMapEntry.value.kind == pjObject &&
				len(lastMapEntry.value.mapData) > 0 && !lastMapEntry.value.keysStartWithDot() {
				// If a top-level condition did match but no sub-condition matched,
				// complain about the sub-condition instead of the top-level condition.
				// This leads to a less confusing error message. For example:
				//
				//   "exports": {
				//     "node": {
				//       "require": "./dist/bwip-js-node.js"
				//     }
				//   },
				//
				// We want the warning to say this:
				//
				//   note: None of the conditions provided ("require") match any of the
				//         currently active conditions ("default", "import", "node")
				//   14 |       "node": {
				//      |               ^
				//
				// We don't want the warning to say this:
				//
				//   note: None of the conditions provided ("browser", "electron", "node")
				//         match any of the currently active conditions ("default", "import", "node")
				//   7 |   "exports": {
				//     |              ^
				//
				// More information: https://github.com/evanw/esbuild/issues/1484
				target = lastMapEntry.value
			}
			keys := make([]string, len(target.mapData))
			for i, p := range target.mapData {
				keys[i] = p.key
			}
			return "", pjStatusUndefinedNoConditionsMatch, pjDebug{
				token:               target.firstToken,
				unmatchedConditions: keys,
			}
		}

		return "", pjStatusUndefined, pjDebug{token: target.firstToken}

	case pjArray:
		if len(target.arrData) == 0 {
			if r.debugLogs != nil {
				r.debugLogs.addNote(fmt.Sprintf("The path %q is set to an empty array", subpath))
			}
			return "", pjStatusNull, pjDebug{token: target.firstToken}
		}
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("Checking for %q in an array", subpath))
			r.debugLogs.increaseIndent()
			defer r.debugLogs.decreaseIndent()
		}
		lastException := pjStatusUndefined
		lastDebug := pjDebug{token: target.firstToken}
		for _, targetValue := range target.arrData {
			// Let resolved be the result, continuing the loop on any Invalid Package Target error.
			resolved, status, debug := r.esmPackageTargetResolve(packageURL, targetValue, subpath, pattern, internal, conditions)
			if status == pjStatusInvalidPackageTarget || status == pjStatusNull {
				lastException = status
				lastDebug = debug
				continue
			}
			if status.isUndefined() {
				continue
			}
			return resolved, status, debug
		}

		// Return or throw the last fallback resolution null return or error.
		return "", lastException, lastDebug

	case pjNull:
		if r.debugLogs != nil {
			r.debugLogs.addNote(fmt.Sprintf("The path %q is set to null", subpath))
		}
		return "", pjStatusNull, pjDebug{token: target.firstToken}
	}

	if r.debugLogs != nil {
		r.debugLogs.addNote(fmt.Sprintf("Invalid package target for path %q", subpath))
	}
	return "", pjStatusInvalidPackageTarget, pjDebug{token: target.firstToken}
}

func esmParsePackageName(packageSpecifier string) (packageName string, packageSubpath string, ok bool) {
	if packageSpecifier == "" {
		return
	}

	slash := strings.IndexByte(packageSpecifier, '/')
	if !strings.HasPrefix(packageSpecifier, "@") {
		if slash == -1 {
			slash = len(packageSpecifier)
		}
		packageName = packageSpecifier[:slash]
	} else {
		if slash == -1 {
			return
		}
		slash2 := strings.IndexByte(packageSpecifier[slash+1:], '/')
		if slash2 == -1 {
			slash2 = len(packageSpecifier[slash+1:])
		}
		packageName = packageSpecifier[:slash+1+slash2]
	}

	if strings.HasPrefix(packageName, ".") || strings.ContainsAny(packageName, "\\%") {
		return
	}

	packageSubpath = "." + packageSpecifier[len(packageName):]
	ok = true
	return
}

func (r resolverQuery) esmPackageExportsReverseResolve(
	query string,
	root pjEntry,
	conditions map[string]bool,
) (bool, string, logger.Range) {
	if root.kind == pjObject && root.keysStartWithDot() {
		if ok, subpath, token := r.esmPackageImportsExportsReverseResolve(query, root, conditions); ok {
			return true, subpath, token
		}
	}

	return false, "", logger.Range{}
}

func (r resolverQuery) esmPackageImportsExportsReverseResolve(
	query string,
	matchObj pjEntry,
	conditions map[string]bool,
) (bool, string, logger.Range) {
	if !strings.HasSuffix(query, "*") {
		for _, entry := range matchObj.mapData {
			if ok, subpath, token := r.esmPackageTargetReverseResolve(query, entry.key, entry.value, esmReverseExact, conditions); ok {
				return true, subpath, token
			}
		}
	}

	for _, expansion := range matchObj.expansionKeys {
		if strings.HasSuffix(expansion.key, "*") {
			if ok, subpath, token := r.esmPackageTargetReverseResolve(query, expansion.key, expansion.value, esmReversePattern, conditions); ok {
				return true, subpath, token
			}
		}

		if ok, subpath, token := r.esmPackageTargetReverseResolve(query, expansion.key, expansion.value, esmReversePrefix, conditions); ok {
			return true, subpath, token
		}
	}

	return false, "", logger.Range{}
}

type esmReverseKind uint8

const (
	esmReverseExact esmReverseKind = iota
	esmReversePattern
	esmReversePrefix
)

func (r resolverQuery) esmPackageTargetReverseResolve(
	query string,
	key string,
	target pjEntry,
	kind esmReverseKind,
	conditions map[string]bool,
) (bool, string, logger.Range) {
	switch target.kind {
	case pjString:
		switch kind {
		case esmReverseExact:
			if query == target.strData {
				return true, key, target.firstToken
			}

		case esmReversePrefix:
			if strings.HasPrefix(query, target.strData) {
				return true, key + query[len(target.strData):], target.firstToken
			}

		case esmReversePattern:
			star := strings.IndexByte(target.strData, '*')
			keyWithoutTrailingStar := strings.TrimSuffix(key, "*")

			// Handle the case of no "*"
			if star == -1 {
				if query == target.strData {
					return true, keyWithoutTrailingStar, target.firstToken
				}
				break
			}

			// Only support tracing through a single "*"
			prefix := target.strData[0:star]
			suffix := target.strData[star+1:]
			if !strings.ContainsRune(suffix, '*') && strings.HasPrefix(query, prefix) {
				if afterPrefix := query[len(prefix):]; strings.HasSuffix(afterPrefix, suffix) {
					starData := afterPrefix[:len(afterPrefix)-len(suffix)]
					return true, keyWithoutTrailingStar + starData, target.firstToken
				}
			}
			break
		}

	case pjObject:
		for _, p := range target.mapData {
			if p.key == "default" || conditions[p.key] {
				if ok, subpath, token := r.esmPackageTargetReverseResolve(query, key, p.value, kind, conditions); ok {
					return true, subpath, token
				}
			}
		}

	case pjArray:
		for _, targetValue := range target.arrData {
			if ok, subpath, token := r.esmPackageTargetReverseResolve(query, key, targetValue, kind, conditions); ok {
				return true, subpath, token
			}
		}
	}

	return false, "", logger.Range{}
}
