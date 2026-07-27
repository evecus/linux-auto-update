package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// UpdateResult holds the result of a check or update run.
type UpdateResult struct {
	HasUpdate      bool
	CurrentVersion string
	LatestVersion  string
	Asset          *GHAsset
}

// CheckUpdate queries GitHub and returns whether a newer version exists.
func CheckUpdate(task *Task, logger func(string)) (*UpdateResult, error) {
	owner, repo, err := ParseOwnerRepo(task.RepoURL)
	if err != nil {
		return nil, err
	}
	logger(fmt.Sprintf("🔎 checking %s/%s ...", owner, repo))

	rel, err := FetchLatestRelease(owner, repo)
	if err != nil {
		return nil, err
	}
	logger(fmt.Sprintf("📋 latest release tag: %s  (current: %s)", rel.TagName, task.CurrentVersion))

	result := &UpdateResult{
		CurrentVersion: task.CurrentVersion,
		LatestVersion:  rel.TagName,
	}

	if task.CurrentVersion == "" {
		result.HasUpdate = true
	} else {
		newer, err := semverCompare(task.CurrentVersion, rel.TagName)
		if err != nil {
			return nil, err
		}
		result.HasUpdate = newer
	}

	if result.HasUpdate {
		asset := BestAsset(rel.Assets, task.FileKeyword)
		if asset == nil {
			return nil, fmt.Errorf("no assets found in release %s", rel.TagName)
		}
		logger(fmt.Sprintf("📎 matched asset: %s (%.1f MB)", asset.Name, float64(asset.Size)/1024/1024))
		result.Asset = asset
	}
	return result, nil
}

// RunUpdate performs a full update for the given task.
func RunUpdate(task *Task, store *Store) {
	logPath := store.LogPath(task.ID)
	logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	defer logFile.Close()

	ts := time.Now().Format("2006-01-02 15:04:05")
	header := fmt.Sprintf("\n========== update started %s ==========\n", ts)
	logFile.WriteString(header)

	logger := func(msg string) {
		line := fmt.Sprintf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
		logFile.WriteString(line)
	}

	setStatus := func(status, errMsg string) {
		store.UpdateTaskField(task.ID, func(t *Task) {
			t.Status = status
			t.LastError = errMsg
			t.LastCheck = time.Now()
		})
	}

	setStatus("checking", "")
	result, err := CheckUpdate(task, logger)
	if err != nil {
		logger(fmt.Sprintf("❌ check failed: %v", err))
		setStatus("error", err.Error())
		return
	}

	// Always save the latest version we learned about
	store.UpdateTaskField(task.ID, func(t *Task) {
		t.LatestVersion = result.LatestVersion
	})

	if !result.HasUpdate {
		logger(fmt.Sprintf("✅ already up-to-date (%s)", result.CurrentVersion))
		setStatus("ok", "")
		return
	}

	logger(fmt.Sprintf("🆕 update available: %s → %s", result.CurrentVersion, result.LatestVersion))
	setStatus("updating", "")

	// Download
	proxyEnabled := store.GetSettings().GithubProxyEnabled
	tmpFile, err := DownloadFile(result.Asset.BrowserDownloadURL, proxyEnabled, logger)
	if err != nil {
		logger(fmt.Sprintf("❌ download failed: %v", err))
		setStatus("error", err.Error())
		return
	}
	defer os.Remove(tmpFile)

	// ---- Package type: download and install via system package manager ----
	if task.UpdateType == UpdateTypePackage {
		// Pre-update command
		if task.PreCmd != "" {
			logger(fmt.Sprintf("⚙ running pre-update: %s", task.PreCmd))
			if out, err := runShell(task.PreCmd); err != nil {
				logger(fmt.Sprintf("❌ pre-update failed: %v\n%s", err, out))
				setStatus("error", fmt.Sprintf("pre-cmd: %v", err))
				return
			} else if out != "" {
				logger("pre-update output: " + out)
			}
		}

		pkgMgr, installCmd, err := detectPackageManager(tmpFile, logger)
		if err != nil {
			logger(fmt.Sprintf("❌ package manager detection failed: %v", err))
			setStatus("error", err.Error())
			return
		}
		logger(fmt.Sprintf("📦 detected package manager: %s", pkgMgr))
		logger(fmt.Sprintf("⚙ installing: %s", installCmd))
		if out, err := runShell(installCmd); err != nil {
			logger(fmt.Sprintf("❌ install failed: %v\n%s", err, out))
			setStatus("error", fmt.Sprintf("install: %v", err))
			return
		} else if out != "" {
			logger("install output: " + out)
		}
		logger("✅ package installed successfully")

		// Post-update command
		if task.PostCmd != "" {
			logger(fmt.Sprintf("⚙ running post-update: %s", task.PostCmd))
			if out, err := runShell(task.PostCmd); err != nil {
				logger(fmt.Sprintf("⚠ post-update failed: %v\n%s", err, out))
			} else if out != "" {
				logger("post-update output: " + out)
			}
		}

		store.UpdateTaskField(task.ID, func(t *Task) {
			t.CurrentVersion = result.LatestVersion
			t.LastUpdate = time.Now()
			t.LastCheck = time.Now()
			t.Status = "ok"
			t.LastError = ""
		})
		logger(fmt.Sprintf("🎉 updated to %s", result.LatestVersion))
		return
	}

	var deployFile string

	if task.UpdateType == UpdateTypeCore {
		// Extract and find the binary
		owner, repo, _ := ParseOwnerRepo(task.RepoURL)
		_ = owner
		binaryPath, err := ExtractAndFindBinary(tmpFile, repo, task.BinaryKeyword, logger)
		if err != nil {
			logger(fmt.Sprintf("❌ extract failed: %v", err))
			setStatus("error", err.Error())
			return
		}
		defer os.RemoveAll(filepath.Dir(binaryPath))

		// Rename
		finalName := task.Rename
		if finalName == "" {
			finalName = filepath.Base(binaryPath)
		}

		renamedPath := filepath.Join(filepath.Dir(binaryPath), finalName)
		if binaryPath != renamedPath {
			if err := os.Rename(binaryPath, renamedPath); err != nil {
				logger(fmt.Sprintf("❌ rename failed: %v", err))
				setStatus("error", err.Error())
				return
			}
		}
		// chmod +x
		if err := os.Chmod(renamedPath, 0755); err != nil {
			logger(fmt.Sprintf("⚠ chmod failed: %v", err))
		}
		deployFile = renamedPath
		logger(fmt.Sprintf("✓ binary ready: %s", finalName))

	} else {
		// Plain file update
		finalName := task.Rename
		if finalName == "" {
			finalName = result.Asset.Name
		}
		renamedPath := filepath.Join(filepath.Dir(tmpFile), finalName)
		if err := os.Rename(tmpFile, renamedPath); err != nil {
			// copy instead
			if err2 := copyFilePath(tmpFile, renamedPath); err2 != nil {
				logger(fmt.Sprintf("❌ rename/copy failed: %v", err))
				setStatus("error", err.Error())
				return
			}
		}
		deployFile = renamedPath
		logger(fmt.Sprintf("✓ file ready: %s", finalName))
	}

	// Pre-update command
	if task.PreCmd != "" {
		logger(fmt.Sprintf("⚙ running pre-update: %s", task.PreCmd))
		if out, err := runShell(task.PreCmd); err != nil {
			logger(fmt.Sprintf("❌ pre-update failed: %v\n%s", err, out))
			setStatus("error", fmt.Sprintf("pre-cmd: %v", err))
			return
		} else {
			if out != "" {
				logger("pre-update output: " + out)
			}
		}
	}

	// Deploy: ensure target dir exists, then move file
	targetDir := task.TargetPath
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		logger(fmt.Sprintf("❌ create target dir failed: %v", err))
		setStatus("error", err.Error())
		return
	}

	destFile := filepath.Join(targetDir, filepath.Base(deployFile))
	logger(fmt.Sprintf("📂 deploying to: %s", destFile))

	// Try rename (same filesystem), fall back to copy+delete
	if err := os.Rename(deployFile, destFile); err != nil {
		if err2 := copyFilePath(deployFile, destFile); err2 != nil {
			logger(fmt.Sprintf("❌ deploy failed: %v", err2))
			setStatus("error", err2.Error())
			return
		}
		os.Remove(deployFile)
	}

	// Ensure exec bit for core type
	if task.UpdateType == UpdateTypeCore {
		os.Chmod(destFile, 0755)
	}

	// Post-update command
	if task.PostCmd != "" {
		logger(fmt.Sprintf("⚙ running post-update: %s", task.PostCmd))
		if out, err := runShell(task.PostCmd); err != nil {
			logger(fmt.Sprintf("⚠ post-update failed: %v\n%s", err, out))
			// not fatal
		} else {
			if out != "" {
				logger("post-update output: " + out)
			}
		}
	}

	// Save new version
	store.UpdateTaskField(task.ID, func(t *Task) {
		t.CurrentVersion = result.LatestVersion
		t.LastUpdate = time.Now()
		t.LastCheck = time.Now()
		t.Status = "ok"
		t.LastError = ""
	})
	logger(fmt.Sprintf("🎉 updated to %s", result.LatestVersion))
}

// detectPackageManager checks the downloaded file extension and available
// package managers on the system, then returns an install command.
func detectPackageManager(pkgFile string, logger func(string)) (string, string, error) {
	lower := strings.ToLower(pkgFile)

	// Determine package format from file extension
	switch {
	case strings.HasSuffix(lower, ".deb"):
		// Prefer apt/apt-get for .deb
		for _, mgr := range [][]string{
			{"apt", "apt install -y " + pkgFile},
			{"apt-get", "apt-get install -y " + pkgFile},
			{"dpkg", "dpkg -i " + pkgFile},
		} {
			if commandExists(mgr[0]) {
				return mgr[0], mgr[1], nil
			}
		}
		return "", "", fmt.Errorf("no suitable package manager found for .deb (tried apt, apt-get, dpkg)")

	case strings.HasSuffix(lower, ".rpm"):
		// Prefer dnf/yum/rpm for .rpm
		for _, mgr := range [][]string{
			{"dnf", "dnf install -y " + pkgFile},
			{"yum", "yum install -y " + pkgFile},
			{"zypper", "zypper --non-interactive install " + pkgFile},
			{"rpm", "rpm -Uvh " + pkgFile},
		} {
			if commandExists(mgr[0]) {
				return mgr[0], mgr[1], nil
			}
		}
		return "", "", fmt.Errorf("no suitable package manager found for .rpm (tried dnf, yum, zypper, rpm)")

	case strings.HasSuffix(lower, ".pkg.tar.zst") ||
		strings.HasSuffix(lower, ".pkg.tar.xz") ||
		strings.HasSuffix(lower, ".pkg.tar.gz"):
		if commandExists("pacman") {
			return "pacman", "pacman -U --noconfirm " + pkgFile, nil
		}
		return "", "", fmt.Errorf("pacman not found; cannot install .pkg.tar.* package")

	default:
		// Unknown extension — try to auto-detect from system
		logger("⚠ file extension not recognised, probing system package managers…")
		if commandExists("apt") {
			return "apt", "apt install -y " + pkgFile, nil
		}
		if commandExists("dnf") {
			return "dnf", "dnf install -y " + pkgFile, nil
		}
		if commandExists("pacman") {
			return "pacman", "pacman -U --noconfirm " + pkgFile, nil
		}
		return "", "", fmt.Errorf("cannot determine package format or package manager for file: %s", filepath.Base(pkgFile))
	}
}

// commandExists returns true if cmd is found in PATH.
func commandExists(cmd string) bool {
	_, err := exec.LookPath(cmd)
	return err == nil
}

func runShell(cmd string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	out, err := c.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func copyFilePath(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, _ := in.Stat()
	mode := os.FileMode(0644)
	if info != nil {
		mode = info.Mode()
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 1<<20)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	return nil
}
