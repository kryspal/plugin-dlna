package main

import "github.com/kryspal/plugin-sdk/i18n"

// messages is the declared string table (change request 0057): en, zh-TW and zh-CN; other languages follow the i18n fallback chain.
var messages = i18n.New(messageTables)

var messageTables = map[string]map[string]string{
	"en": {
		"desc": "Plays your videos on a television or other DLNA receiver on your home network. The receiver fetches the video straight from this server, so nothing is transcoded. Automatic discovery needs the server and the receiver on the same network (use host networking when you run in Docker). A receiver that cannot be found can be added by its IP address under Playback → Cast → Add by address, which also works with ordinary Docker networking.",
	},
	"zh-TW": {
		"desc": "把影片投到家中網路上的電視或其他 DLNA 接收端播放。接收端直接從這台伺服器取得影片，不經過轉碼。自動發現需要伺服器和接收端在同一個網路（用 Docker 時要用 host 網路）；找不到的接收端可以到「播放 → 投屏」按 IP 位址「手動加入」，一般的 Docker 網路也能用。",
	},
	"zh-CN": {
		"desc": "把视频投到家中网络上的电视或其他 DLNA 接收端播放。接收端直接从这台服务器获取视频，不经过转码。自动发现需要服务器和接收端在同一个网络（用 Docker 时要用 host 网络）；找不到的接收端可以到“播放 → 投屏”按 IP 地址“手动添加”，一般的 Docker 网络也能用。",
	},
}
