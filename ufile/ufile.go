package ufile
import (
	"sync"
	"github.com/go-fsnotify/fsnotify"
	"time"
	"path/filepath"
	"strings"
	"net/url"
	"os"
	"fmt"
	"io"
	"net/http"
)

/*

文件加载功能
允许加载本地文件及远程文件。

本地文件修改后自动重新加载，远程文件定时重新加载。
*/

type UserData interface{}

type UFile struct {
	basePath      string            //绝对路径
	dirs          map[string]int    // 目录内需要监听的文件数
	files         map[string]*uFile // map 修改需要使用 rwm。uFile是只读的，运行中不允许修改。
	rwm           sync.RWMutex
	downM         sync.Mutex        // down 下载锁，防止多次重复下载。
	watcher       *fsnotify.Watcher //监听本地文件修改。实际监听的是hosts所在的目录
									// configCond sync.Cond
	exited        bool              // 是否已退出
	checkInterval time.Duration     //http检测间隔
	ResChan       chan (*Res)       // 结果信道，加载器关闭时信道也会关闭。
}

type Res struct {
	RawPath  string        // 用户输入的原始路径
	Path     string        // 实际使用的路径（绝对路径）
	Userdata UserData      // 用户数据
	Rc       io.ReadCloser // 文件 ，出错时为空
	Err      error         // 是否出现了错误
}

type uFile struct {
	RawPath        string        // 输入的原始路径
	Path           string        //host 文件路径(绝对路径)、本地或远程url(http、https)
	Local          bool          //本地？远程
	updateInterval time.Duration // 更新间隔
	utime          time.Time     // 下次更新日期(读取、修改需要持有锁)
	userdata       UserData
}

// NewUFile 新建文件加载器
// basePath 本地文件的基本路径
// checkInterval 网络文件的最小检测间隔
func NewUFile(basePath string, checkInterval time.Duration) (*UFile, error) {
	basePath = strings.TrimSpace(basePath)
	if basePath == "" {
		basePath = "."
	}

	basePath, err := filepath.Abs(basePath)
	if err != nil {
		return nil, err
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	uf := UFile{
		basePath:basePath,
		dirs:make(map[string]int),
		files:make(map[string]*uFile),
		watcher:watcher,
		checkInterval:checkInterval,
		ResChan:make(chan *Res, 2),
	}
	go uf.loop()

	return &uf, nil
}

// 是否已退出
// 是线程安全的，需要小心死锁
func (u*UFile)isExited() bool {
	u.rwm.RLock()
	defer u.rwm.RUnlock()
	return u.exited
}


// 关闭文件加载器
// 同时会关闭 ResChan 。
func (u*UFile)Close() {
	if u.isExited() {
		return
	}

	u.rwm.Lock()
	defer u.rwm.Unlock()

	u.exited = true
	u.watcher.Close()
	u.watcher = nil
	u.dirs = nil
	u.files = nil
	close(u.ResChan)
}

// 下载文件
// 允许本地文件及远程文件
// 可能会阻塞到结果信道。
func (u*UFile)down(f*uFile) (rerr error) {
	// 只允许一个 down进程运行，防止第一次添加时 Add 函数调用 + loop调用下载造成重复下载2次。
	u.downM.Lock()
	defer u.downM.Unlock()

	now := time.Now()
	var utime time.Time

	func() {
		u.rwm.RLock()
		defer u.rwm.RUnlock()
		utime = f.utime
	}()

	// 再次检查是否需要更新，防止第一次添加时 Add 函数调用 + loop调用下载造成重复下载2次。
	if utime.After(now) {
		return
	}

	res := Res{
		RawPath:f.RawPath,
		Path:f.Path,
		Userdata:f.userdata,
	}

	if f.Local == true {
		f, err := os.Open(f.Path)

		if err != nil {
			rerr = err
			res.Err = err
		} else {
			res.Rc = f
		}
	}else {
		r, err := http.Get(f.Path)

		if err != nil {
			rerr = err
			res.Err = err
		}else {
			res.Rc = r.Body
		}
	}

	func() {
		u.rwm.Lock()
		defer u.rwm.Unlock()
		f.utime = now.Add(f.updateInterval)
	}()

	func() {
		defer func() {_ = recover()}()
		u.ResChan <- &res
	}()

	return res.Err
}


// 添加文件（允许本地路径及远程路径）
// 即使本地文件不存在只要目录存在就会安全返回，通过信道返回文件不存在的提示。等文件创建时会再次通过信道返回正确的内容。
// url 只要格式正确就会返回，之后会启动新协程下载文件，成功失败都会通过信道返回结果。
// 注意：添加本地文件时文件所在的目录必须存在，不存在会尝试创建，创建失败会添加失败，返回错误。
func (u*UFile)Add(path string, updateInterval time.Duration, userdata UserData) error {
	local := false
	dir := ""
	rawPath := path

	path = strings.TrimSpace(path)
	lPath := strings.ToLower(path)

	if strings.HasPrefix(lPath, "http://") || strings.HasPrefix(lPath, "https://") {
		_, err := url.Parse(path)
		if err != nil {
			return err
		}
	} else {
		local = true
		if filepath.IsAbs(path) == false {
			path = filepath.Join(u.basePath, path)
		}
		dir = filepath.Dir(path)

		if err := os.MkdirAll(dir, 755); err != nil {
			return err
		}
	}


	uf := uFile{
		RawPath:rawPath,
		Path:path,
		Local:local,
		updateInterval:updateInterval,
		userdata:userdata,
	}

	u.rwm.Lock()
	defer u.rwm.Unlock()

	if local == true {
		if u.dirs[dir] == 0 {
			u.watcher.Add(dir)
		}
		u.dirs[dir] += 1
	}

	u.files[path] = &uf

	// 手工启动第一次下载
	// 本地文件需要这一步，网络文件为了立刻就下载也需要这一步。
	go u.down(&uf)

	return nil
}

// 移除文件
// 移除文件变更监听。
func (u*UFile)Remove(path string) error {
	dir := ""
	local := true

	path = strings.TrimSpace(path)

	if lPath := strings.ToLower(path);
	strings.HasPrefix(lPath, "http://") || strings.HasPrefix(lPath, "https://") {
		local = false
	} else {
		if filepath.IsAbs(path) == false {
			path = filepath.Join(u.basePath, path)
		}
		dir = filepath.Dir(path)
	}

	u.rwm.Lock()
	defer u.rwm.Unlock()

	// 判断是否存在
	if _, ok := u.files[path]; ok != true {
		return fmt.Errorf("指定的 path(%v) 不存在。", path)
	}

	// 本地文件需要移除文件夹变更监听
	if local {
		if u.dirs[dir] == 1 {
			if err := u.watcher.Remove(dir); err != nil {
				return err
			}
			delete(u.dirs, dir)
		}else {
			u.dirs[dir] -= 1
		}
	}

	delete(u.files, path)
	return nil
}


// 更新循环
// 处理 hosts 本地文件更新 及 http 定期更新。
func (u*UFile)loop() {
	wg := sync.WaitGroup{}

	wg.Add(2)
	go func() {
		u.loop_http()
		wg.Done()
	}()
	go func() {
		u.loop_watcher()
		wg.Done()
	}()

	wg.Wait()
}

// 监听 hosts 文件修改
func (u*UFile)loop_watcher() {
	u.rwm.RLock()
	watcher := u.watcher
	u.rwm.RUnlock()

	for event := range watcher.Events {
		// 不管是创建、编辑还是重命名，直接匹配路径，发现路径正确就直接重新加载。

		var uf *uFile

		func() {
			u.rwm.RLock()
			defer u.rwm.RUnlock()
			uf = u.files[event.Name]
		}()

		if uf != nil && uf.Local == true {
			if _, err := os.Stat(uf.Path); err == nil {
				u.down(uf)
			}
		}
	}
}

// 循环更新 http、https hosts文件
func (u*UFile)loop_http() {
	for {
		if u.isExited() {
			return
		}

		var checkInterval time.Duration
		now := time.Now()

		// 临时保存需要更新数据
		data := make([]*uFile, 0)

		// 取出需要更新的内容，尽量短的使用写锁。
		func() {
			u.rwm.RLock()
			defer u.rwm.RUnlock()
			checkInterval = u.checkInterval

			for _, f := range u.files {
				if f.Local == false && now.After(f.utime) {
					data = append(data, f)
				}
			}
		}()

		for _, f := range data {
			u.down(f)
		}

		time.Sleep(checkInterval)
	}
}


