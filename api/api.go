package api

// IE: Single responsibility, break the Handler away from this file
// IE: for example create api_handler.go (New() + packageHandler()) and dependency_resolver.go (rest of funcs)

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

type npmPackageMetaResponse struct {
	Versions map[string]npmPackageResponse `json:"versions"`
}

// IE: why expose NpmPackageVersion outside the api package if we are only using api.New() ???
// IE: why were 2 types with identical structures in the same package prior to this PR????
// IE: we can optimize the 2 structs as following:
//
//	type package struct {
//			Name         string            `json:"name"`
//			Version      string            `json:"version"`
//	}
//
//	type npmPackageResponse struct {
//			Package			package
//			Dependencies 	map[string]string `json:"dependencies"`
//	}
//
//	type npmPackageVersion struct {
//			Package			package
//			Dependencies 	map[string]*NpmPackageVersion `json:"dependencies"`
//	}

type npmPackageResponse struct {
	Name         string            `json:"name"`
	Version      string            `json:"version"`
	Dependencies map[string]string `json:"dependencies"`
}

type NpmPackageVersion struct {
	Name         string                        `json:"name" deepcopier:"field:Name"`
	Version      string                        `json:"version"  deepcopier:"field:Version"`
	Dependencies map[string]*NpmPackageVersion `json:"dependencies" deepcopier:"field:Dependencies"`
	sync.RWMutex `deepcopier:"skip"`
}

// IE: use log for logging instead of simple Println for extra features (i.e. timestamp)
var errorLogger *log.Logger
var debugLogger *log.Logger

// IE: use a WaitGroup to process each recursive resolveDependencies() request asynchronously
var wg sync.WaitGroup

// IE: cache the last request for instant response on repeated identical requests
var lastRequest map[string][]byte

// IE: debug counter for start/end resolveDependencies()
var goroutineCount WaitGroupCount

var rootPkg *NpmPackageVersion

var scannedPkgs map[string]*NpmPackageVersion
var scannedPkgsMutex = sync.RWMutex{}

var copiedDeps map[string]string

func New() http.Handler {
	// IE: use log for logging instead of fmt for extra features (i.e. timestamp)
	errorLogger = log.New(os.Stdout, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
	debugLogger = log.New(os.Stdout, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile)

	// IE: in case we need to limit resource CPU Load
	// runtime.GOMAXPROCS(runtime.NumCPU() * 0.75)
	goroutineCount = WaitGroupCount{}

	router := mux.NewRouter()
	router.Handle("/package/{package}/{version}", http.HandlerFunc(packageHandler))

	// IE: cache the last request for instant response on repeated identical requests
	lastRequest = make(map[string][]byte)

	scannedPkgs = make(map[string]*NpmPackageVersion)
	copiedDeps = make(map[string]string)

	return router
}

func packageHandler(w http.ResponseWriter, r *http.Request) {
	// IE: start timestamp for debugging purposes
	start := time.Now()

	var toWrite []byte
	if cached, found := lastRequest[r.RequestURI]; found {
		// IE: request is identical to previous one, return from cached response
		toWrite = cached
	} else {
		vars := mux.Vars(r)

		// IE: someone might use this func at some point with bad params
		// IE: check for 'package' and 'version' presence in the 'vars' map
		pkgName, ok := vars["package"]
		if !ok {
			errorLogger.Println("Package name not found:", r.RequestURI)
			return
		}
		pkgVersion, ok := vars["version"]
		if !ok {
			errorLogger.Println("Package version not found:", r.RequestURI)
			return
		}

		// IE: NpmPackageVersion also has a 'version' attribute, should pass 'pkgVersion' into rootPkg
		newEmptyDeps := make(map[string]*NpmPackageVersion)
		newEmptyDeps[uuid.NewString()] = nil
		rootPkg = &NpmPackageVersion{Name: pkgName, Version: pkgVersion, Dependencies: newEmptyDeps}

		// IE: send task to WaitGroup to perform it asynchronously, new goroutine for each dependency found
		wg.Add(1)
		go resolveDependencies(rootPkg, pkgVersion)
		wg.Wait()

		debugLogger.Println("Changing node names...")

		for k := range copiedDeps {
			delete(copiedDeps, k)
		}
		changeNodeNames(rootPkg)

		stringified, err := json.MarshalIndent(rootPkg, "", "  ")
		if err != nil {
			// IE: use log for logging instead of fmt for extra features (i.e. timestamp)
			errorLogger.Println(err.Error())
			w.WriteHeader(500)
			return
		}
		toWrite = stringified
		lastRequest[r.RequestURI] = stringified
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)

	debugLogger.Println("Writing json...")
	// Ignoring ResponseWriter errors
	_, _ = w.Write(toWrite)

	// IE: log time spent retrieving full dependency tree for each request
	debugLogger.Println("Request for", r.RequestURI, "completed in", (time.Since(start)))

	goroutineCount.Done()
}

// IE: need to send each package retrieval on a separate thread
func resolveDependencies(pkg *NpmPackageVersion, versionConstraint string) {
	// IE: signal that the goroutine is done to WaitGroup before each goroutine ends
	defer wg.Done()

	// IE: debug counter
	goroutineCount.Add(1)
	debugLogger.Println("Starting goroutine", goroutineCount.GetCount())

	pkgMeta, err := fetchPackageMeta(pkg.Name)
	if err != nil {
		// IE: log the error
		errorLogger.Println("Could not fetch package meta for", pkg.Name)
		return
	}
	concreteVersion, err := highestCompatibleVersion(versionConstraint, pkgMeta)
	if err != nil {
		// IE: log the error
		errorLogger.Println("Could not find highest compatible version for", pkg.Name)
		return
	}
	pkg.Version = concreteVersion

	npmPkg, err := fetchPackage(pkg.Name, pkg.Version)
	if err != nil {
		// IE: log the error
		errorLogger.Println("Could not fetch package dependency", pkg.Name, "version", pkg.Version)
		return
	}

	// IE: need some sort of protection against circular dependencies
	// i.e. trucolor 4.0.4 cannot be retrieved, npmjs eventually closes the connection and sends GOAWAY
	for dependencyName, dependencyVersionConstraint := range npmPkg.Dependencies {

		// IE: get dependencies for nodes already scanned, NOT WORKING ATM
		var cachedDeps, emptyNpmPackageVersion NpmPackageVersion
		getCachedDeps(dependencyName, dependencyVersionConstraint, rootPkg, &cachedDeps)
		if cachedDeps.Name != emptyNpmPackageVersion.Name && cachedDeps.Version != emptyNpmPackageVersion.Version {
			pkg.Dependencies = cachedDeps.Dependencies
		} else {
			newEmptyDeps := make(map[string]*NpmPackageVersion)
			dep := &NpmPackageVersion{Name: dependencyName, Version: dependencyVersionConstraint, Dependencies: newEmptyDeps}

			pkg.Dependencies[uuid.NewString()] = dep
			if len(pkg.Dependencies) > 0 {
				// IE: send each each package dependency retrieval on a new goroutine
				wg.Add(1)
				go resolveDependencies(dep, dependencyVersionConstraint)
			}

		}
	}

	// IE: debug counter
	debugLogger.Println("Ending goroutine", goroutineCount.GetCount())
	goroutineCount.Add(-1)

	scannedPkgsMutex.Lock()
	scannedPkgs[fmt.Sprintf("%s%s", pkg.Name, pkg.Version)] = pkg
	scannedPkgsMutex.Unlock()

	debugLogger.Println("Scanned package", fmt.Sprintf("%s%s", pkg.Name, pkg.Version))
}

// IE: this func should be checking asynchronously if a certain package
// has already been scanned for dependency during current request and would return that node
// so that its dependencies could be simply copied to another tree level where that dependency resides
func getCachedDeps(name string, version string, curNode *NpmPackageVersion, cachedDeps *NpmPackageVersion) {
	scannedPkgsMutex.RLock()
	cachedPkg, exist := scannedPkgs[fmt.Sprintf("%s%s", name, strings.Trim(version, "^"))]
	scannedPkgsMutex.RUnlock()

	if exist {
		*cachedDeps = *cachedPkg

		debugLogger.Println("Found duplicate: ", cachedPkg.Name, cachedPkg.Version)

		return
	}
}

func copyDeps(src *NpmPackageVersion, dest *NpmPackageVersion) {
	if src.Dependencies == nil || len(src.Dependencies) == 0 {
		return
	}

	newEmptyDeps := make(map[string]*NpmPackageVersion)
	dest = &NpmPackageVersion{Name: src.Name, Version: src.Version, Dependencies: newEmptyDeps}

	for k, dep := range src.Dependencies {
		if dep != nil {
			newEmptyDeps := make(map[string]*NpmPackageVersion)
			curNode := &NpmPackageVersion{Name: dep.Name, Version: dep.Version, Dependencies: newEmptyDeps}
			copyDeps(dep, curNode)
			dest.Dependencies[k] = curNode
		}
	}
}

func changeNodeNames(node *NpmPackageVersion) {
	if node.Dependencies == nil || len(node.Dependencies) == 0 {
		debugLogger.Println("no more deps")
		return
	}

	debugLogger.Println("retrieving deps for", node.Name, node.Version)
	for nodeName, dep := range node.Dependencies {
		node.set(uuid.NewString(), dep)
		node.delete(nodeName)

		if dep != nil {
			if _, ok := copiedDeps[dep.Name+dep.Version]; !ok {
				copiedDeps[dep.Name+dep.Version] = dep.Version
				changeNodeNames(dep)
			} else {
				dep.Dependencies = map[string]*NpmPackageVersion{}
			}
		}
	}
}

func (r *NpmPackageVersion) get(key string) *NpmPackageVersion {
	r.RLock()
	defer r.RUnlock()
	return r.Dependencies[key]
}

func (r *NpmPackageVersion) set(key string, value *NpmPackageVersion) {
	r.Lock()
	defer r.Unlock()
	r.Dependencies[key] = value
}

func (r *NpmPackageVersion) delete(key string) {
	r.Lock()
	defer r.Unlock()
	delete(r.Dependencies, key)
}

func highestCompatibleVersion(constraintStr string, versions *npmPackageMetaResponse) (string, error) {
	constraint, err := semver.NewConstraint(constraintStr)
	if err != nil {
		return "", err
	}
	if versions == nil {
		errorLogger.Println("nil versions for ")
	}
	filtered := filterCompatibleVersions(constraint, versions)

	// IE: why sort then compare len to 0 instead of the other way around?
	if len(filtered) == 0 {
		return "", errors.New("no compatible versions found")
	}

	sort.Sort(filtered)

	return filtered[len(filtered)-1].String(), nil
}

func filterCompatibleVersions(constraint *semver.Constraints, pkgMeta *npmPackageMetaResponse) semver.Collection {
	var compatible semver.Collection
	for version := range pkgMeta.Versions {
		semVer, err := semver.NewVersion(version)
		if err != nil {
			continue
		}
		if constraint.Check(semVer) {
			compatible = append(compatible, semVer)
		}
	}
	return compatible
}

func fetchPackage(name, version string) (*npmPackageResponse, error) {
	resp, err := http.Get(fmt.Sprintf("https://registry.npmjs.org/%s/%s", name, version))
	if err != nil {
		return nil, err
	}

	// IE: I would honestly close the stream right after io.ReadAll
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// IE: log the error
		errorLogger.Println("Could not read response body for package", name, "version", version)
		return nil, err
	}

	var parsed npmPackageResponse
	_ = json.Unmarshal(body, &parsed)
	return &parsed, nil
}

func fetchPackageMeta(p string) (*npmPackageMetaResponse, error) {
	resp, err := http.Get(fmt.Sprintf("https://registry.npmjs.org/%s", p))
	if err != nil {
		// IE: log the error
		errorLogger.Println("Failed call on https://registry.npmjs.org/", p, err)
		return nil, err
	}

	// IE: I would honestly close the stream right after io.ReadAll
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		// IE: log the error
		errorLogger.Println("Could not read package meta for package", p, resp.Body, err)
		return nil, err
	}

	var parsed npmPackageMetaResponse
	// IE: no need to convert to byte slice since 'body' is already returned as []byte from io.ReadAll
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, err
	}

	return &parsed, nil
}
