package buildpacks

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/johnewart/archiver"
	. "github.com/yourbase/yb/plumbing"
	. "github.com/yourbase/yb/types"
)

//https://archive.apache.org/dist/dart/dart-3/3.3.3/binaries/apache-dart-3.3.3-bin.tar.gz
var DART_DIST_MIRROR = "https://storage.googleapis.com/dart-archive/channels/stable/release/{{.Version}}/sdk/dartsdk-{{.OS}}-{{.Arch}}-release.zip"

type DartBuildTool struct {
	BuildTool
	version string
	spec    BuildToolSpec
}

func NewDartBuildTool(toolSpec BuildToolSpec) DartBuildTool {
	tool := DartBuildTool{
		version: toolSpec.Version,
		spec:    toolSpec,
	}

	return tool
}

func (bt DartBuildTool) DownloadUrl() string {
	opsys := OS()
	arch := Arch()
	extension := "zip"

	if arch == "amd64" {
		arch = "x64"
	}

	if opsys == "darwin" {
		opsys = "macos"
	}

	version := bt.Version()

	data := struct {
		OS        string
		Arch      string
		Version   string
		Extension string
	}{
		opsys,
		arch,
		version,
		extension,
	}

	url, _ := TemplateToString(DART_DIST_MIRROR, data)

	return url
}

func (bt DartBuildTool) MajorVersion() string {
	parts := strings.Split(bt.version, ".")
	return parts[0]
}

func (bt DartBuildTool) Version() string {
	return bt.version
}

func (bt DartBuildTool) InstallDir() string {
	return filepath.Join(bt.spec.SharedCacheDir, "dart", bt.Version())
}

func (bt DartBuildTool) DartDir() string {
	return filepath.Join(bt.InstallDir(), "dart-sdk")
}

func (bt DartBuildTool) Setup() error {
	dartDir := bt.DartDir()
	cmdPath := filepath.Join(dartDir, "bin")
	PrependToPath(cmdPath)

	return nil
}

// TODO, generalize downloader
func (bt DartBuildTool) Install() error {
	dartDir := bt.DartDir()
	installDir := bt.InstallDir()

	if _, err := os.Stat(dartDir); err == nil {
		fmt.Printf("Dart v%s located in %s!\n", bt.Version(), dartDir)
	} else {
		fmt.Printf("Will install Dart v%s into %s\n", bt.Version(), dartDir)
		downloadUrl := bt.DownloadUrl()

		fmt.Printf("Downloading Dart from URL %s...\n", downloadUrl)
		localFile, err := DownloadFileWithCache(downloadUrl)
		if err != nil {
			fmt.Printf("Unable to download: %v\n", err)
			return err
		}
		err = archiver.Unarchive(localFile, installDir)
		if err != nil {
			fmt.Printf("Unable to decompress: %v\n", err)
			return err
		}
	}

	return nil
}
