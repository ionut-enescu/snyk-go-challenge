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
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/gorilla/mux"
)

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

func New() http.Handler {
	// IE: use log for logging instead of fmt for extra features (i.e. timestamp)
	errorLogger = log.New(os.Stdout, "ERROR: ", log.Ldate|log.Ltime|log.Lshortfile)
	debugLogger = log.New(os.Stdout, "DEBUG: ", log.Ldate|log.Ltime|log.Lshortfile)

	// IE: in case we need to limit resource CPU Load
	// runtime.GOMAXPROCS(runtime.NumCPU() * 0.75)
	goroutineCount = WaitGroupCount{wg, 0}

	router := mux.NewRouter()
	router.Handle("/package/{package}/{version}", http.HandlerFunc(packageHandler))

	// IE: cache the last request for instant response on repeated identical requests
	lastRequest = make(map[string][]byte)
	return router
}

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
	Name         string                        `json:"name"`
	Version      string                        `json:"version"`
	Dependencies map[string]*NpmPackageVersion `json:"dependencies"`
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
		rootPkg = &NpmPackageVersion{Name: pkgName, Version: pkgVersion, Dependencies: map[string]*NpmPackageVersion{}}

		// IE: send task to WaitGroup to perform it asynchronously, new goroutine for each dependency found
		wg.Add(1)
		go resolveDependencies(rootPkg, pkgVersion)
		wg.Wait()

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
		dep := &NpmPackageVersion{Name: dependencyName, Version: dependencyVersionConstraint, Dependencies: map[string]*NpmPackageVersion{}}

		// // IE: get dependencies for nodes already scanned, NOT WORKING ATM
		// var cachedDeps *NpmPackageVersion
		// getCachedDeps(dependencyName, dependencyVersionConstraint, rootPkg, cachedDeps)
		// if cachedDeps != nil {
		// 	pkg.Dependencies = cachedDeps.Dependencies
		// } else {

		pkg.Dependencies[dependencyName] = dep
		if len(pkg.Dependencies) > 0 {
			// IE: send each each package dependency retrieval on a new goroutine
			wg.Add(1)
			go resolveDependencies(dep, dependencyVersionConstraint)
		}

		// }
	}

	// IE: debug counter
	debugLogger.Println("Ending goroutine", goroutineCount.GetCount())
	goroutineCount.Add(-1)
}

// IE: this func should be checking asynchronously if a certain package
// has already been scanned for dependency during current request and would return that node
// so that its dependencies could be simply copied to another tree level where that dependency resides
// TODO: NOT WORKING ATM, FIX IT
func getCachedDeps(name string, version string, curNode *NpmPackageVersion, cachedDeps *NpmPackageVersion) {
	if len(curNode.Dependencies) == 0 {
		return
	}

	for _, dep := range curNode.Dependencies {
		if name == dep.Name && version == dep.Version {
			cachedDeps = dep
		}

		getCachedDeps(name, version, dep, cachedDeps)
	}
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
	// IE: why assume that last version returned by filterCompatibleVersions() is also the latest? what if someone changes that method at some point?
	// IE: should look inside the whole 'filtered' collection for the latest version
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
