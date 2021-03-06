package main

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	chitrace "gopkg.in/DataDog/dd-trace-go.v1/contrib/go-chi/chi.v5"
	"gopkg.in/DataDog/dd-trace-go.v1/ddtrace/tracer"
	"gopkg.in/DataDog/dd-trace-go.v1/profiler"

	"github.com/bradfitz/gomemcache/memcache"
	gsm "github.com/bradleypeabody/gorilla-sessions-memcache"
	"github.com/go-chi/chi/v5"
	_ "github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
	"github.com/jmoiron/sqlx"
)

var (
	db             *sqlx.DB
	memcacheClient *memcache.Client
	store          *gsm.MemcacheStore
)

const (
	postsPerPage       = 20
	ISO8601Format      = "2006-01-02T15:04:05-07:00"
	UploadLimit        = 10 * 1024 * 1024 // 10mb
	imageDirName       = "/home/isucon/private_isu/webapp/image/"
	DatadogServiceName = "private-isu"
	DatadogEnv         = "myenv"
)

type User struct {
	ID          int       `db:"id"`
	AccountName string    `db:"account_name"`
	Passhash    string    `db:"passhash"`
	Authority   int       `db:"authority"`
	DelFlg      int       `db:"del_flg"`
	CreatedAt   time.Time `db:"created_at"`
}

type Post struct {
	ID           int       `db:"id"`
	UserID       int       `db:"user_id"`
	Imgdata      []byte    `db:"imgdata"`
	Body         string    `db:"body"`
	Mime         string    `db:"mime"`
	CreatedAt    time.Time `db:"created_at"`
	CommentCount int
	Comments     []Comment
	User         User
	CSRFToken    string
}

type Comment struct {
	ID        int       `db:"id"`
	PostID    int       `db:"post_id"`
	UserID    int       `db:"user_id"`
	Comment   string    `db:"comment"`
	CreatedAt time.Time `db:"created_at"`
	User      User
}

func init() {
	memdAddr := os.Getenv("ISUCONP_MEMCACHED_ADDRESS")
	if memdAddr == "" {
		memdAddr = "localhost:11211"
	}
	memcacheClient = memcache.New(memdAddr)
	store = gsm.NewMemcacheStore(memcacheClient, "iscogram_", []byte("sendagaya"))
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
}

func dbInitialize() {
	sqls := []string{
		"DELETE FROM users WHERE id > 1000",
		"DELETE FROM posts WHERE id > 10000",
		"DELETE FROM comments WHERE id > 100000",
		"UPDATE users SET del_flg = 0",
		"UPDATE users SET del_flg = 1 WHERE id % 50 = 0",
	}

	for _, sql := range sqls {
		db.Exec(sql)
	}
}

func tryLogin(accountName, password string) *User {
	u := User{}
	err := db.Get(&u, "SELECT * FROM users WHERE account_name = ? AND del_flg = 0", accountName)
	if err != nil {
		return nil
	}

	if calculatePasshash(u.AccountName, password) == u.Passhash {
		return &u
	} else {
		return nil
	}
}

func validateUser(accountName, password string) bool {
	return regexp.MustCompile(`\A[0-9a-zA-Z_]{3,}\z`).MatchString(accountName) &&
		regexp.MustCompile(`\A[0-9a-zA-Z_]{6,}\z`).MatchString(password)
}

// ?????????Go????????????????????????????????????????????????????????????????????????OS??????????????????????????????????????????????????????
// ????????????PHP???escapeshellarg?????????????????????????????????
// cf: http://jp2.php.net/manual/ja/function.escapeshellarg.php
func escapeshellarg(arg string) string {
	return "'" + strings.Replace(arg, "'", "'\\''", -1) + "'"
}

func digest(src string) string {
	// openssl????????????????????????????????? (stdin)= ?????????????????????????????????
	out, err := exec.Command("/bin/bash", "-c", `printf "%s" `+escapeshellarg(src)+` | openssl dgst -sha512 | sed 's/^.*= //'`).Output()
	if err != nil {
		log.Print(err)
		return ""
	}

	return strings.TrimSuffix(string(out), "\n")
}

func calculateSalt(accountName string) string {
	return digest(accountName)
}

func calculatePasshash(accountName, password string) string {
	return digest(password + ":" + calculateSalt(accountName))
}

func getSession(r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isuconp-go.session")

	return session
}

func getSessionUser(r *http.Request) User {
	session := getSession(r)
	uid, ok := session.Values["user_id"]
	if !ok || uid == nil {
		return User{}
	}

	u := User{}

	err := db.Get(&u, "SELECT * FROM `users` WHERE `id` = ?", uid)
	// u, err := getUser(uid.(int64)) // FIXME: use cache
	if err != nil {
		return User{}
	}

	return u
}

func getFlash(w http.ResponseWriter, r *http.Request, key string) string {
	session := getSession(r)
	value, ok := session.Values[key]

	if !ok || value == nil {
		return ""
	} else {
		delete(session.Values, key)
		session.Save(r, w)
		return value.(string)
	}
}

func getCommentsMap(posts []Post) (map[int][]Comment, error) {
	var postIds = make([]int, 0, len(posts))
	for _, p := range posts {
		postIds = append(postIds, p.ID)
	}
	var allComments []Comment
	query, params, err := sqlx.In("SELECT * FROM `comments` where post_id in (?) ORDER BY `created_at` DESC", postIds)
	if err != nil {
		return nil, err
	}
	err = db.Select(&allComments, query, params...)
	if err != nil {
		return nil, err
	}
	var commentsMap = make(map[int][]Comment)
	for _, c := range allComments {
		commentsMap[c.PostID] = append(commentsMap[c.PostID], c)
	}
	return commentsMap, nil
}

// FIXME: CUD?????????????????????????????????????????????
func getUser(userId int) (User, error) {
	var user = User{}
	it, err := memcacheClient.Get(fmt.Sprintf("user_%d", userId))
	if err == nil {
		err := json.Unmarshal(it.Value, &user)
		if err != nil {
			return User{}, err
		}
	} else {
		err := db.Get(&user, "SELECT * FROM `users` WHERE `id` = ?", userId)
		if err != nil {
			return User{}, err
		}
		j, err := json.Marshal(user)
		if err != nil {
			return User{}, err
		}
		memcacheClient.Set(&memcache.Item{
			Key:   fmt.Sprintf("user_%d", userId),
			Value: j,
		})
	}
	return user, nil
}

func setCommentUser(comments []Comment) ([]Comment, error) {
	var mcKeys = make([]string, 0, len(comments))
	for _, c := range comments {
		mcKeys = append(mcKeys, fmt.Sprintf("user_%d", c.UserID))
	}
	mcItems, err := memcacheClient.GetMulti(mcKeys)
	if err == nil {
		return nil, err
	}
	var noCacheUserIds = make([]int, 0, len(comments))
	for _, c := range comments {
		mcKey := fmt.Sprintf("user_%d", c.UserID)
		mcItem, ok := mcItems[mcKey]
		if ok {
			err := json.Unmarshal(mcItem.Value, &c.User)
			if err != nil {
				return nil, err
			}
		} else {
			noCacheUserIds = append(noCacheUserIds, c.UserID)
		}
	}
	if len(noCacheUserIds) == 0 {
		return comments, nil
	}

	userQuery, params, err := sqlx.In("SELECT * FROM `users` WHERE `id` in (?)", noCacheUserIds)
	if err != nil {
		return nil, err
	}
	var noCacheUsers = make([]User, 0, len(noCacheUserIds))
	err = db.Select(&noCacheUsers, userQuery, params...)
	if err != nil {
		return nil, err
	}
	var noCacheUserMap = make(map[int]User)
	for _, u := range noCacheUsers {
		noCacheUserMap[u.ID] = u

		j, err := json.Marshal(u)
		if err != nil {
			return nil, err
		}
		memcacheClient.Set(&memcache.Item{
			Key:   fmt.Sprintf("user_%d", u.ID),
			Value: j,
		})
	}
	for _, c := range comments {
		u, ok := noCacheUserMap[c.User.ID]
		if ok {
			c.User = u
		}
	}
	return comments, nil
}

func makePosts(results []Post, csrfToken string, isAllComments bool) ([]Post, error) {
	var posts []Post

	commentsMap, err := getCommentsMap(results)
	if err != nil {
		return nil, err
	}

	for _, p := range results {
		// err = db.Get(&p.CommentCount, "SELECT COUNT(*) AS `count` FROM `comments` WHERE `post_id` = ?", p.ID)  // FIXME: ?????????
		// if err != nil {
		//  	return nil, err
		// }
		// query := "SELECT * FROM `comments` WHERE `post_id` = ? ORDER BY `created_at` DESC"  // FIXME: ?????????
		// if !isAllComments {
		//	query += " LIMIT 3"
		// }
		var comments []Comment
		// err = db.Select(&comments, query, p.ID)
		// if err != nil {
		//	return nil, err
		// }
		comments = commentsMap[p.ID]
		p.CommentCount = len(comments)
		if isAllComments && len(comments) > 3 {
			comments = comments[0:2]
		}

		if len(comments) > 0 {
			comments, err = setCommentUser(comments)
			if err != nil {
				return nil, err
			}
		}

		// for i := 0; i < len(comments); i++ {
		// 	err := db.Get(&comments[i].User, "SELECT * FROM `users` WHERE `id` = ?", comments[i].UserID) // FIXME: ?????????
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// }

		// reverse
		for i, j := 0, len(comments)-1; i < j; i, j = i+1, j-1 {
			comments[i], comments[j] = comments[j], comments[i]
		}

		p.Comments = comments

		// err = db.Get(&p.User, "SELECT * FROM `users` WHERE `id` = ?", p.UserID) // FIXME: ?????????
		// if err != nil {
		// 	return nil, err
		// }

		p.CSRFToken = csrfToken

		// if p.User.DelFlg == 0 {
		// 	posts = append(posts, p)
		// }
		posts = append(posts, p)
		// if len(posts) >= postsPerPage {
		// 	break
		// }
	}

	return posts, nil
}

func imageURL(p Post) string {
	ext := ""
	if p.Mime == "image/jpeg" {
		ext = ".jpg"
	} else if p.Mime == "image/png" {
		ext = ".png"
	} else if p.Mime == "image/gif" {
		ext = ".gif"
	}

	return "/image/" + strconv.Itoa(p.ID) + ext
}

func isLogin(u User) bool {
	return u.ID != 0
}

func getCSRFToken(r *http.Request) string {
	session := getSession(r)
	csrfToken, ok := session.Values["csrf_token"]
	if !ok {
		return ""
	}
	return csrfToken.(string)
}

func secureRandomStr(b int) string {
	k := make([]byte, b)
	if _, err := crand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", k)
}

func getTemplPath(filename string) string {
	return path.Join("templates", filename)
}

func getInitialize(w http.ResponseWriter, r *http.Request) {
	dbInitialize()
	w.WriteHeader(http.StatusOK)
}

func getLogin(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)

	if isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("login.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{me, getFlash(w, r, "notice")})
}

func postLogin(w http.ResponseWriter, r *http.Request) { // FIXME: slow
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	u := tryLogin(r.FormValue("account_name"), r.FormValue("password"))

	if u != nil {
		session := getSession(r)
		session.Values["user_id"] = u.ID
		session.Values["csrf_token"] = secureRandomStr(16)
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
	} else {
		session := getSession(r)
		session.Values["notice"] = "????????????????????????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/login", http.StatusFound)
	}
}

func getRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("register.html")),
	).Execute(w, struct {
		Me    User
		Flash string
	}{User{}, getFlash(w, r, "notice")})
}

func postRegister(w http.ResponseWriter, r *http.Request) {
	if isLogin(getSessionUser(r)) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	accountName, password := r.FormValue("account_name"), r.FormValue("password")

	validated := validateUser(accountName, password)
	if !validated {
		session := getSession(r)
		session.Values["notice"] = "?????????????????????3?????????????????????????????????6??????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	exists := 0
	// ????????????????????????????????????????????????????????????????????????????????????????????????
	db.Get(&exists, "SELECT 1 FROM users WHERE `account_name` = ?", accountName)

	if exists == 1 {
		session := getSession(r)
		session.Values["notice"] = "???????????????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/register", http.StatusFound)
		return
	}

	query := "INSERT INTO `users` (`account_name`, `passhash`) VALUES (?,?)"
	result, err := db.Exec(query, accountName, calculatePasshash(accountName, password))
	if err != nil {
		log.Print(err)
		return
	}

	session := getSession(r)
	uid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}
	session.Values["user_id"] = uid
	session.Values["csrf_token"] = secureRandomStr(16)
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)

	http.Redirect(w, r, "/", http.StatusFound)
}

func getIndex(w http.ResponseWriter, r *http.Request) { // FIXME: slow
	me := getSessionUser(r)

	results := []Post{}

	// err := db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` ORDER BY `created_at` DESC")
	err := db.Select(&results, "SELECT STRAIGHT_JOIN p.id, p.user_id, p.body, p.mime, p.created_at FROM `posts` p JOIN `users` u ON p.user_id = u.id WHERE u.del_flg = 0 ORDER BY p.created_at DESC LIMIT 20") // FIXME: postsPerPage
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("index.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Posts     []Post
		Me        User
		CSRFToken string
		Flash     string
	}{posts, me, getCSRFToken(r), getFlash(w, r, "notice")})
}

func getAccountName(w http.ResponseWriter, r *http.Request) {
	accountName := chi.URLParam(r, "accountName")
	user := User{}

	err := db.Get(&user, "SELECT * FROM `users` WHERE `account_name` = ? AND `del_flg` = 0", accountName)
	if err != nil {
		log.Print(err)
		return
	}

	if user.ID == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}

	// err = db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `user_id` = ? ORDER BY `created_at` DESC", user.ID)
	err = db.Select(&results, "SELECT STRAIGHT_JOIN p.id, p.user_id, p.body, p.mime, p.created_at FROM `posts` p JOIN `users` u ON p.user_id = u.id WHERE u.del_flg = 0 AND p.user_id = ? ORDER BY p.created_at DESC LIMIT 20", user.ID) // FIXME: postsPerPage, slow
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	commentCount := 0
	err = db.Get(&commentCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}

	postIDs := []int{}
	err = db.Select(&postIDs, "SELECT `id` FROM `posts` WHERE `user_id` = ?", user.ID)
	if err != nil {
		log.Print(err)
		return
	}
	postCount := len(postIDs)

	commentedCount := 0
	if postCount > 0 {
		s := []string{}
		for range postIDs {
			s = append(s, "?")
		}
		placeholder := strings.Join(s, ", ")

		// convert []int -> []interface{}
		args := make([]interface{}, len(postIDs))
		for i, v := range postIDs {
			args[i] = v
		}

		err = db.Get(&commentedCount, "SELECT COUNT(*) AS count FROM `comments` WHERE `post_id` IN ("+placeholder+")", args...)
		if err != nil {
			log.Print(err)
			return
		}
	}

	me := getSessionUser(r)

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("user.html"),
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Posts          []Post
		User           User
		PostCount      int
		CommentCount   int
		CommentedCount int
		Me             User
	}{posts, user, postCount, commentCount, commentedCount, me})
}

func getPosts(w http.ResponseWriter, r *http.Request) { // FIXME: slow
	m, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		log.Print(err)
		return
	}
	maxCreatedAt := m.Get("max_created_at")
	if maxCreatedAt == "" {
		return
	}

	t, err := time.Parse(ISO8601Format, maxCreatedAt)
	if err != nil {
		log.Print(err)
		return
	}

	results := []Post{}
	// err = db.Select(&results, "SELECT `id`, `user_id`, `body`, `mime`, `created_at` FROM `posts` WHERE `created_at` <= ? ORDER BY `created_at` DESC", t.Format(ISO8601Format))
	err = db.Select(&results, "SELECT STRAIGHT_JOIN p.id, p.user_id, p.body, p.mime, p.created_at FROM `posts` p JOIN `users` u ON p.user_id = u.id WHERE u.del_flg = 0 AND p.created_at <= ? ORDER BY p.created_at DESC LIMIT 20", t.Format(ISO8601Format)) // FIXME: postsPerPage
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), false)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("posts.html").Funcs(fmap).ParseFiles(
		getTemplPath("posts.html"),
		getTemplPath("post.html"),
	)).Execute(w, posts)
}

func getPostsID(w http.ResponseWriter, r *http.Request) {
	pidStr := chi.URLParam(r, "id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	results := []Post{}
	// err = db.Select(&results, "SELECT * FROM `posts` WHERE `id` = ?", pid)
	err = db.Select(&results, "SELECT p.id, p.user_id, p.mime, p.body, p.created_at FROM `posts` p JOIN `users` u ON p.user_id = u.id WHERE u.del_flg = 0 AND p.id = ?", pid)
	if err != nil {
		log.Print(err)
		return
	}

	posts, err := makePosts(results, getCSRFToken(r), true)
	if err != nil {
		log.Print(err)
		return
	}

	if len(posts) == 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	p := posts[0]

	me := getSessionUser(r)

	fmap := template.FuncMap{
		"imageURL": imageURL,
	}

	template.Must(template.New("layout.html").Funcs(fmap).ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("post_id.html"),
		getTemplPath("post.html"),
	)).Execute(w, struct {
		Post Post
		Me   User
	}{p, me})
}

func postIndex(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		session := getSession(r)
		session.Values["notice"] = "?????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	mime := ""
	ext := ""
	if file != nil {
		// ?????????Content-Type?????????????????????????????????????????????
		contentType := header.Header["Content-Type"][0]
		if strings.Contains(contentType, "jpeg") {
			mime = "image/jpeg"
			ext = "jpg"
		} else if strings.Contains(contentType, "png") {
			mime = "image/png"
			ext = "png"
		} else if strings.Contains(contentType, "gif") {
			mime = "image/gif"
			ext = "gif"
		} else {
			session := getSession(r)
			session.Values["notice"] = "??????????????????????????????jpg???png???gif????????????"
			session.Save(r, w)

			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
	}

	filedata, err := io.ReadAll(file)
	if err != nil {
		log.Print(err)
		return
	}

	if len(filedata) > UploadLimit {
		session := getSession(r)
		session.Values["notice"] = "??????????????????????????????????????????"
		session.Save(r, w)

		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	query := "INSERT INTO `posts` (`user_id`, `mime`, `imgdata`, `body`) VALUES (?,?,?,?)"
	result, err := db.Exec(
		query,
		me.ID,
		mime,
		"", // dummy
		// filedata,
		r.FormValue("body"),
	)
	if err != nil {
		log.Print(err)
		return
	}

	pid, err := result.LastInsertId()
	if err != nil {
		log.Print(err)
		return
	}

	fullName := imageDirName + strconv.FormatInt(pid, 10) + "." + ext
	f, err := os.Create(fullName)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	_, err = f.Write(filedata)
	if err != nil {
		log.Fatal(err)
	}

	http.Redirect(w, r, "/posts/"+strconv.FormatInt(pid, 10), http.StatusFound)
}

func getImage(w http.ResponseWriter, r *http.Request) {
	pidStr := chi.URLParam(r, "id")
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	ext := chi.URLParam(r, "ext")

	fullName := imageDirName + pidStr + "." + ext
	var data []byte
	if _, err := os.Stat(fullName); err == nil {
		data, err = ioutil.ReadFile(fullName)
		if err != nil {
			log.Fatal(err)
		}

		if ext == "jpg" ||
			ext == "png" ||
			ext == "gif" {
			w.Header().Set("Content-Type", "image/"+ext)
			_, err := w.Write(data)
			if err != nil {
				log.Print(err)
				return
			}
			return
		}

	} else {
		post := Post{}
		err = db.Get(&post, "SELECT * FROM `posts` WHERE `id` = ?", pid)
		if err != nil {
			log.Print(err)
			return
		}

		f, err := os.Create(fullName)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()

		data = post.Imgdata
		_, err = f.Write(data)
		if err != nil {
			log.Fatal(err)
		}

		if ext == "jpg" && post.Mime == "image/jpeg" ||
			ext == "png" && post.Mime == "image/png" ||
			ext == "gif" && post.Mime == "image/gif" {
			w.Header().Set("Content-Type", post.Mime)
			_, err := w.Write(data)
			if err != nil {
				log.Print(err)
				return
			}
			return
		}
	}

	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusNotFound)
}

func postComment(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	postID, err := strconv.Atoi(r.FormValue("post_id"))
	if err != nil {
		log.Print("post_id?????????????????????")
		return
	}

	query := "INSERT INTO `comments` (`post_id`, `user_id`, `comment`) VALUES (?,?,?)"
	_, err = db.Exec(query, postID, me.ID, r.FormValue("comment"))
	if err != nil {
		log.Print(err)
		return
	}

	http.Redirect(w, r, fmt.Sprintf("/posts/%d", postID), http.StatusFound)
}

func getAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	users := []User{}
	err := db.Select(&users, "SELECT * FROM `users` WHERE `authority` = 0 AND `del_flg` = 0 ORDER BY `created_at` DESC")
	if err != nil {
		log.Print(err)
		return
	}

	template.Must(template.ParseFiles(
		getTemplPath("layout.html"),
		getTemplPath("banned.html")),
	).Execute(w, struct {
		Users     []User
		Me        User
		CSRFToken string
	}{users, me, getCSRFToken(r)})
}

func postAdminBanned(w http.ResponseWriter, r *http.Request) {
	me := getSessionUser(r)
	if !isLogin(me) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}

	if me.Authority == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if r.FormValue("csrf_token") != getCSRFToken(r) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	query := "UPDATE `users` SET `del_flg` = ? WHERE `id` = ?"

	err := r.ParseForm()
	if err != nil {
		log.Print(err)
		return
	}

	for _, id := range r.Form["uid[]"] {
		db.Exec(query, 1, id)
	}

	http.Redirect(w, r, "/admin/banned", http.StatusFound)
}

func main() {
	err := profiler.Start(
		profiler.WithService(DatadogServiceName),
		profiler.WithEnv(DatadogEnv),
		profiler.WithProfileTypes(
			profiler.CPUProfile,
			profiler.HeapProfile,
			// The profiles below are disabled by default to keep overhead
			// low, but can be enabled as needed.

			// profiler.BlockProfile,
			// profiler.MutexProfile,
			// profiler.GoroutineProfile,
		),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer profiler.Stop()

	host := os.Getenv("ISUCONP_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	port := os.Getenv("ISUCONP_DB_PORT")
	if port == "" {
		port = "3306"
	}
	_, err = strconv.Atoi(port)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCONP_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCONP_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCONP_DB_PASSWORD")
	dbname := os.Getenv("ISUCONP_DB_NAME")
	if dbname == "" {
		dbname = "isuconp"
	}

	dsn := fmt.Sprintf(
		"%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=true&loc=Local&interpolateParams=true",
		user,
		password,
		host,
		port,
		dbname,
	)

	db, err = sqlx.Open("mysql", dsn)
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	// Start the tracer
	tracer.Start(
		tracer.WithService(DatadogServiceName),
		tracer.WithEnv(DatadogEnv),
		// tracer.WithRuntimeMetrics(),
	)
	defer tracer.Stop()

	r := chi.NewRouter()

	r.Use(chitrace.Middleware(chitrace.WithServiceName(DatadogServiceName)))

	r.Get("/initialize", getInitialize)
	r.Get("/login", getLogin)
	r.Post("/login", postLogin)
	r.Get("/register", getRegister)
	r.Post("/register", postRegister)
	r.Get("/logout", getLogout)
	r.Get("/", getIndex)
	r.Get("/posts", getPosts)
	r.Get("/posts/{id}", getPostsID)
	r.Post("/", postIndex)
	r.Get("/image/{id}.{ext}", getImage)
	r.Post("/comment", postComment)
	r.Get("/admin/banned", getAdminBanned)
	r.Post("/admin/banned", postAdminBanned)
	r.Get(`/@{accountName:[a-zA-Z]+}`, getAccountName)
	r.Get("/*", func(w http.ResponseWriter, r *http.Request) {
		http.FileServer(http.Dir("../public")).ServeHTTP(w, r)
	})

	log.Fatal(http.ListenAndServe(":8080", r))
}
