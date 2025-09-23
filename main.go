// go-wallpaper-tray - Windows 10 daily wallpaper changer from wallscloud.net
// Features:
// - At 09:00 local time each day the program requests https://wallscloud.net/ru/wallpapers/random
//   and uses XPath //*[@id="main"]/div[4]/div[2]/figure[1]/div/a to get the <a href="..."> link.
// - Appends "/1600x900/download" to the href and downloads the image.
// - Converts downloaded image to BMP and sets as desktop wallpaper on Windows 10.
// - If started after 09:00, checks whether today's wallpaper was already set (stores last date in a file).
// - Runs in the system tray. Menu items: "Force change now", "Exit".
// NOTE: Minimal error handling. Improve for production use.

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/image/bmp"

	"github.com/antchfx/htmlquery"
	"github.com/getlantern/systray"
)

const (
	siteURL           = "https://wallscloud.net/ru/wallpapers/random"
	xpathSelector     = "//*[@id=\"main\"]/div[4]/div[2]/figure[1]/div/a"
	imageSuffix       = "/1600x900/download"
	appFolderName     = "GoWallpaperTray"
	lastDateFileName  = "last_update.txt"
	wallpaperFileName = "wallpaper.bmp"
)

//go:embed icon.ico
var iconData []byte

func main() {
	if runtime.GOOS != "windows" {
		fmt.Println("This program is intended to run on Windows.")
		return
	}

	// Ensure app dir
	appDir, err := getAppDir()
	if err != nil {
		fmt.Println("failed to get app dir:", err)
		return
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		fmt.Println("failed to create app dir:", err)
		return
	}

	// ⚡ systray.Run блокирующий — запускаем его прямо здесь
	systray.Run(onReady, onExit)
}

func onReady() {
	if len(iconData) > 0 {
		systray.SetIcon(iconData)
	}
	systray.SetTitle("GoWallpaper")
	systray.SetTooltip("Daily wallpaper changer from wallscloud.net")

	mForce := systray.AddMenuItem("Force change now", "Download and set wallpaper now")
	mExit := systray.AddMenuItem("Exit", "Exit the program")

	// Run background worker for scheduling
	ctx, cancel := context.WithCancel(context.Background())
	go scheduleWorker(ctx)

	// menu handling
	go func() {
		for {
			select {
			case <-mForce.ClickedCh:
				go func() {
					if err := changeWallpaperNow(); err != nil {
						showMessagePopup("Error", err.Error())
					} else {
						showMessagePopup("Wallpaper updated", "Wallpaper changed successfully")
					}
				}()
			case <-mExit.ClickedCh:
				cancel()
				systray.Quit()
				return
			}
		}
	}()
}

func onExit() {
	fmt.Println("Exiting…")
	os.Exit(0) // ⚡ гарантированное завершение процесса
}

// scheduleWorker triggers change at 09:00 local time daily and also performs initial check when app starts.
func scheduleWorker(ctx context.Context) {
	appDir, _ := getAppDir()
	lastDatePath := filepath.Join(appDir, lastDateFileName)

	now := time.Now()
	today9 := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, now.Location())
	if now.After(today9) || now.Equal(today9) {
		if !wasUpdatedToday(lastDatePath) {
			_ = changeWallpaperNow()
		}
	}

	for {
		next := next9AM(time.Now())
		d := time.Until(next)
		select {
		case <-time.After(d):
			_ = changeWallpaperNow()
		case <-ctx.Done():
			return
		}
	}
}

func next9AM(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, now.Location())
	if !now.Before(t) {
		t = t.Add(24 * time.Hour)
	}
	return t
}

func changeWallpaperNow() error {
	appDir, err := getAppDir()
	if err != nil {
		return err
	}
	lastDatePath := filepath.Join(appDir, lastDateFileName)
	wallPath := filepath.Join(appDir, wallpaperFileName)

	href, err := fetchRandomWallpaperHref(siteURL, xpathSelector)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(href, "http") {
		href = strings.TrimRight(siteURL, "/") + "/" + strings.TrimLeft(href, "/")
	}
	dlURL := strings.TrimRight(href, "/") + imageSuffix

	tmpFile, err := downloadToTemp(dlURL)
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile)

	if err := convertToBMP(tmpFile, wallPath); err != nil {
		return err
	}

	if err := setWallpaperWindows(wallPath); err != nil {
		return err
	}

	today := time.Now().Format("2006-01-02")
	_ = os.WriteFile(lastDatePath, []byte(today), 0o644)

	return nil
}

func fetchRandomWallpaperHref(url, xpath string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("bad status: %s", resp.Status)
	}
	doc, err := htmlquery.Parse(resp.Body)
	if err != nil {
		return "", err
	}
	n := htmlquery.FindOne(doc, xpath)
	if n == nil {
		return "", errors.New("xpath didn't return node")
	}
	href := htmlquery.SelectAttr(n, "href")
	if href == "" {
		href = htmlquery.SelectAttr(n, "data-href")
	}
	return href, nil
}

func downloadToTemp(url string) (string, error) {
	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download bad status: %s", resp.Status)
	}
	tmp, err := os.CreateTemp("", "wall_*")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	_, err = io.Copy(tmp, resp.Body)
	if err != nil {
		return "", err
	}
	return tmp.Name(), nil
}

func convertToBMP(srcPath, dstPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	out, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer out.Close()
	return bmp.Encode(out, img)
}

func setWallpaperWindows(path string) error {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("SystemParametersInfoW")
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	ret, _, callErr := proc.Call(
		uintptr(20), // SPI_SETDESKWALLPAPER
		uintptr(0),
		uintptr(unsafe.Pointer(p)),
		uintptr(0x01|0x02), // SPIF_UPDATEINIFILE | SPIF_SENDWININICHANGE
	)
	if ret == 0 {
		if callErr != nil {
			return callErr
		}
		return errors.New("SystemParametersInfoW failed")
	}
	return nil
}

func wasUpdatedToday(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == time.Now().Format("2006-01-02")
}

func getAppDir() (string, error) {
	appdata := os.Getenv("APPDATA")
	if appdata == "" {
		return "", errors.New("APPDATA not set")
	}
	return filepath.Join(appdata, appFolderName), nil
}

func showMessagePopup(title, msg string) {
	fmt.Println(title+":", msg)
}
