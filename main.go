package room // import "m7s.live/plugin/room/v4"
import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
	"go.uber.org/zap"
	. "m7s.live/engine/v4"
	"m7s.live/engine/v4/common"
	"m7s.live/engine/v4/config"
	"m7s.live/engine/v4/track"
	"m7s.live/engine/v4/util"
)

type User struct {
	Subscriber
	StreamPath string
	Token      string `json:"-" yaml:"-"`
	Room       *Room  `json:"-" yaml:"-"`
	net.Conn   `json:"-" yaml:"-"`
	writeLock  sync.Mutex
}

func (u *User) OnEvent(event any) {
	switch v := event.(type) {
	case *track.Data[[]byte]:
		go v.Play(u.IO, func(data *common.DataFrame[[]byte]) error {
			u.writeLock.Lock()
			defer u.writeLock.Unlock()
			return wsutil.WriteServerText(u.Conn, data.Data)
		})
	default:
		u.Subscriber.OnEvent(event)
	}
}
func (u *User) Send(event string, data any) {
	if u.Conn != nil {
		u.writeLock.Lock()
		defer u.writeLock.Unlock()
		j, err := json.Marshal(map[string]any{"event": event, "data": data})
		if err == nil {
			wsutil.WriteServerText(u.Conn, j)
		}
	}
}

type Room struct {
	Publisher `json:"-" yaml:"-"`
	Users     util.Map[string, *User]
	track     *track.Data[[]byte]
}

var Rooms util.Map[string, *Room]

//go:embed default.yaml
var defaultYaml DefaultYaml

type RoomConfig struct {
	DefaultYaml
	config.Subscribe
	config.HTTP
	AppName string            `default:"room"`
	Size    int               `default:"20"` //房间大小
	Private map[string]string //私密房间 key房间号，value密码
	Verify  struct {
		URL    string
		Method string
		Header map[string]string
	}
	lock sync.RWMutex
}

var plugin = InstallPlugin(&RoomConfig{
	DefaultYaml: defaultYaml,
})

func (rc *RoomConfig) OnEvent(event any) {
	switch v := event.(type) {
	case SEpublish:
		args := v.Target.Publisher.GetPublisher().Args
		token := args.Get("token")
		ss := strings.Split(token, ":")
		if len(ss) != 3 {
			return
		}
		roomId := ss[0]
		userId := ss[1]
		if roomId != "" && Rooms.Has(roomId) {
			room := Rooms.Get(roomId)
			if room.Users.Has(userId) {
				user := room.Users.Get(userId)
				if user.Token == token {
					user.StreamPath = v.Target.Path
					data, _ := json.Marshal(map[string]any{"event": "publish", "data": v.Target.Path, "userId": user.ID})
					room.track.Push(data)
				}
			}
		}
	}
}

func (rc *RoomConfig) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ss := strings.Split(r.URL.Path, "/")[1:]
	var roomId, userId, token string
	if len(ss) == 2 {
		roomId = ss[0]
		userId = ss[1]
	} else {
		http.Error(w, "invalid url", http.StatusBadRequest)
		return
	}
	if rc.Verify.URL != "" {
		req, _ := http.NewRequest(rc.Verify.Method, rc.Verify.URL, nil)
		req.Header = r.Header
		res, _ := http.DefaultClient.Do(req)
		if res.StatusCode != 200 {
			http.Error(w, "verify failed", http.StatusForbidden)
		}
	}
	if rc.Private != nil {
		rc.lock.RLock()
		pass, ok := rc.Private[roomId]
		rc.lock.Unlock()
		if ok {
			if pass != r.URL.Query().Get("password") {
				http.Error(w, "password wrong", http.StatusForbidden)
				return
			}
		}
	}
	var room *Room
	if !Rooms.Has(roomId) {
		room = &Room{}
		room.ID = roomId
		if plugin.Publish(rc.AppName+"/"+roomId, room) == nil {
			Rooms.Add(roomId, room)
			room.track = track.NewDataTrack[[]byte]("data")
			room.track.Locker = &sync.Mutex{}
			room.track.Attach(room.Stream)
		} else {
			http.Error(w, "room already exist", http.StatusBadRequest)
			return
		}
	} else {
		room = Rooms.Get(roomId)
	}
	if room.Users.Has(userId) {
		http.Error(w, "user exist", http.StatusBadRequest)
		return
	}
	conn, _, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer conn.Close()
	token = fmt.Sprintf("%s:%s:%s", roomId, userId, uuid.NewString())
	user := &User{Room: room, Conn: conn, Token: token}
	user.ID = userId
	if err = plugin.Subscribe(rc.AppName+"/"+room.ID, user); err == nil {
		data, _ := json.Marshal(map[string]any{"event": "userjoin", "data": user})
		room.track.Push(data)
		room.Users.Add(userId, user)
		user.Send("joined", map[string]any{"token": token, "userList": room.Users.ToList()})
		defer func() {
			user.Stop(zap.Error(err))
			room.Users.Delete(userId)
			if room.Users.Len() == 0 {
				room.track.Dispose()
				room.Stop()
				Rooms.Delete(roomId)
			}
		}()
	} else {
		return
	}
	var msg []byte
	var op ws.OpCode
	for {
		msg, op, err = wsutil.ReadClientData(conn)
		if op == ws.OpClose || err != nil {
			data, _ := json.Marshal(map[string]any{"event": "userleave", "userId": userId})
			room.track.Push(data)
			return
		}
		data, _ := json.Marshal(map[string]any{"event": "msg", "data": string(msg), "userId": userId})
		room.track.Push(data)
	}
}
