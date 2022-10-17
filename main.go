package room // import "m7s.live/plugin/room/v4"
import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
	"github.com/google/uuid"
	. "m7s.live/engine/v4"
	"m7s.live/engine/v4/common"
	"m7s.live/engine/v4/config"
	"m7s.live/engine/v4/track"
	"m7s.live/engine/v4/util"
)

type User struct {
	Subscriber
	StreamPath string
	Token      string `json:"-"`
	Room       *Room  `json:"-"`
	net.Conn   `json:"-"`
	writeLock  sync.Mutex
}

func (u *User) OnEvent(event any) {
	switch v := event.(type) {
	case *track.Data:
		u.AddTrack(v)
		go v.Play(u.IO, func(data any) error {
			u.writeLock.Lock()
			defer u.writeLock.Unlock()
			return wsutil.WriteServerText(u.Conn, data.(*common.DataFrame[any]).Value.([]byte))
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
	Publisher `json:"-"`
	Users     util.Map[string, *User]
	track     *track.Data
}

var Rooms = util.Map[string, *Room]{Map: make(map[string]*Room)}

type RoomConfig struct {
	config.HTTP
	AppName string
	Size    int               //房间大小
	Private map[string]string //私密房间 key房间号，value密码
	Verify  struct {
		URL    string
		Method string
		Header map[string]string
	}
	lock sync.RWMutex
}

var plugin = InstallPlugin(&RoomConfig{
	Size:    20,
	AppName: "room",
})

func (rc *RoomConfig) OnEvent(event any) {
	switch v := event.(type) {
	case SEpublish:
		args := v.Stream.Publisher.GetIO().Args
		roomId := args.Get("roomId")
		userId := args.Get("userId")
		token := args.Get("token")
		if roomId != "" && Rooms.Has(roomId) {
			room := Rooms.Get(roomId)
			if room.Users.Has(userId) {
				user := room.Users.Get(userId)
				if user.Token == token {
					user.StreamPath = v.Stream.Path
					data, _ := json.Marshal(map[string]any{"event": "publish", "data": v.Stream.Path, "userId": user.ID})
					room.track.Push(data)
				}
			}
		}
	}
}

func (rc *RoomConfig) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ss := strings.Split(r.URL.Path, "/")[2:]
	var roomId, userId, token string
	token = uuid.NewString()
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
		room = &Room{Users: util.Map[string, *User]{Map: make(map[string]*User)}}
		room.ID = roomId
		if plugin.Publish(rc.AppName+"/"+roomId, room) == nil {
			Rooms.Add(roomId, room)
			room.track = room.Stream.NewDataTrack(&sync.Mutex{})
			room.track.Name = "data"
			room.Stream.AddTrack(room.track)
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
	user := &User{Room: room, Conn: conn, Token: token}
	user.ID = userId
	user.Send("joined", token)
	user.Send("userlist", room.Users.ToList())
	data, _ := json.Marshal(map[string]any{"event": "userjoin", "data": user})
	room.track.Push(data)
	room.Users.Add(userId, user)
	if plugin.Subscribe(rc.AppName+"/"+room.ID, user) == nil {

	}
	defer func() {
		user.Stop()
		conn.Close()
		room.Users.Delete(userId)
		Rooms.Lock()
		defer Rooms.Unlock()
		if room.Users.Len() == 0 {
			room.track.Dispose()
			room.Stop()
			delete(Rooms.Map, roomId)
		}
	}()
	for {
		msg, op, err := wsutil.ReadClientData(conn)
		if op == ws.OpClose || err != nil {
			data, _ := json.Marshal(map[string]any{"event": "userleave", "userId": userId})
			room.track.Push(data)
			return
		}
		data, _ := json.Marshal(map[string]any{"event": "msg", "data": string(msg), "userId": userId})
		room.track.Push(data)
	}
}
