package rtspv2

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/blackbeans/vdk/codec/rtph264"
	"github.com/blackbeans/vdk/utils/bits/pio"
	"github.com/pion/rtp"
	"html"
	"io"
	"log"
	"math/rand"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/blackbeans/vdk/av"
	"github.com/blackbeans/vdk/codec"
	"github.com/blackbeans/vdk/codec/aacparser"
	"github.com/blackbeans/vdk/codec/h264parser"
	"github.com/blackbeans/vdk/codec/h265parser"
	"github.com/blackbeans/vdk/format/rtsp/sdp"
	"github.com/pion/rtcp"
)

const (
	SignalStreamRTPStop = iota
	SignalCodecUpdate
)

const (
	VIDEO = "video"
	AUDIO = "audio"
)

const (
	RTPHeaderSize = 12
)
const (
	DESCRIBE = "DESCRIBE"
	OPTIONS  = "OPTIONS"
	PLAY     = "PLAY"
	SETUP    = "SETUP"
	TEARDOWN = "TEARDOWN"
)

type RTSPClient struct {
	control             string
	seq                 int
	session             string
	realm               string
	nonce               string
	username            string
	password            string
	startVideoTS        int64
	startAudioTS        int64
	videoID             int
	audioID             int
	videoIDX            int8
	audioIDX            int8
	mediaSDP            []sdp.Media
	SDPRaw              []byte
	conn                net.Conn
	connRW              *bufio.ReadWriter
	pURL                *url.URL
	headers             map[string]string
	Signals             chan int
	OutgoingProxyQueue  chan *[]byte
	OutgoingPacketQueue chan *av.Packet
	clientDigest        bool
	clientBasic         bool
	fuStarted           bool
	options             RTSPClientOptions
	//缓存RTP包数据FU-A分片包的中间数据
	BufferRtpPacket   *bytes.Buffer
	vps               []byte
	sps               []byte
	pps               []byte
	CodecData         []av.CodecData
	AudioTimeLine     time.Duration
	AudioTimeScale    int64
	audioCodec        av.CodecType
	videoCodec        av.CodecType
	PreAudioTS        int64
	PreVideoTS        int64
	PreSequenceNumber int
	FPS               int
}

type RTSPClientOptions struct {
	Debug            bool
	RtpOverUdp       bool // udp 或者 tcp
	URL              string
	DialTimeout      time.Duration
	ReadWriteTimeout time.Duration
	DisableAudio     bool
	OutgoingProxy    bool
}

func Dial(options RTSPClientOptions) (*RTSPClient, error) {
	client := &RTSPClient{
		headers:             make(map[string]string),
		Signals:             make(chan int, 100),
		OutgoingProxyQueue:  make(chan *[]byte, 3000),
		OutgoingPacketQueue: make(chan *av.Packet, 3000),
		BufferRtpPacket:     bytes.NewBuffer([]byte{}),
		videoID:             -1,
		audioID:             -2,
		videoIDX:            -1,
		audioIDX:            -2,
		options:             options,
		AudioTimeScale:      8000,
	}
	client.headers["User-Agent"] = "Lavf58.20.100"
	err := client.parseURL(html.UnescapeString(client.options.URL))
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("tcp", client.pURL.Host, client.options.DialTimeout)
	if err != nil {
		return nil, err
	}
	err = conn.SetDeadline(time.Now().Add(client.options.ReadWriteTimeout))
	if err != nil {
		return nil, err
	}
	client.conn = conn
	client.connRW = bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))

	err = client.request(OPTIONS, nil, client.pURL.String(), false, false, nil)
	if err != nil {
		return nil, err
	}

	err = client.request(DESCRIBE, map[string]string{"Accept": "application/sdp"}, client.pURL.String(), false, false, nil)
	if err != nil {
		return nil, err
	}
	var ch int
	for idx, i2 := range client.mediaSDP {
		if (i2.AVType != VIDEO && i2.AVType != AUDIO) || (client.options.DisableAudio && i2.AVType == AUDIO) {
			continue
		}

		//默认都用TCP传输
		if !options.RtpOverUdp {
			i2.MediaProtocol = "RTP/AVP/TCP"
		}
		transport := ""
		//不是TCP传输就是UDP
		if i2.MediaProtocol != "RTP/AVP/TCP" {
			//偶数用于RTP报文   奇数用于RTCP
			rtpPort := (rand.Intn((65535-10000)/2) * 2) + 10000
			client.mediaSDP[idx].ClientPort = strconv.Itoa(rtpPort) + "-" + strconv.Itoa(rtpPort+1)
			client.mediaSDP[idx].MediaProtocol = "RTP/AVP/UDP"
			transport = i2.MediaProtocol +
				";unicast;client_port=" + client.mediaSDP[idx].ClientPort
		} else {
			transport = i2.MediaProtocol + ";unicast;interleaved=" + strconv.Itoa(ch) + "-" + strconv.Itoa(ch+1)
		}

		err = client.request(SETUP, map[string]string{"Transport": transport},
			client.ControlTrack(i2.Control), false, false, &client.mediaSDP[idx])
		if err != nil {
			return nil, err
		}
		if i2.AVType == VIDEO {
			if i2.Type == av.H264 {
				if nil != i2.SpropParameterSets && len(i2.SpropParameterSets) > 1 {
					if codecData, err := h264parser.NewCodecDataFromSPSAndPPS(i2.SpropParameterSets[0], i2.SpropParameterSets[1]); err == nil {
						client.sps = i2.SpropParameterSets[0]
						client.pps = i2.SpropParameterSets[1]
						client.CodecData = append(client.CodecData, codecData)
					}
				} else {
					//client.CodecData = append(client.CodecData, h264parser.CodecData{})
				}
				client.FPS = i2.FPS
				client.videoCodec = av.H264
			} else if i2.Type == av.H265 {
				if len(i2.SpropVPS) > 1 && len(i2.SpropSPS) > 1 && len(i2.SpropPPS) > 1 {
					if codecData, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(i2.SpropVPS, i2.SpropSPS, i2.SpropPPS); err == nil {
						client.vps = i2.SpropVPS
						client.sps = i2.SpropSPS
						client.pps = i2.SpropPPS
						client.CodecData = append(client.CodecData, codecData)
					}
				} else {
					client.CodecData = append(client.CodecData, h265parser.CodecData{})
				}
				client.videoCodec = av.H265
			} else {
				client.Println("SDP Video Codec Type Not Supported", i2.Type)
			}
			client.videoIDX = int8(len(client.CodecData) - 1)
			client.videoID = ch
		}
		if i2.AVType == AUDIO {
			client.audioID = ch
			var CodecData av.AudioCodecData
			switch i2.Type {
			case av.AAC:
				CodecData, err = aacparser.NewCodecDataFromMPEG4AudioConfigBytes(i2.Config)
				if err == nil {
					client.Println("Audio AAC bad config")
				}
			case av.OPUS:
				var cl av.ChannelLayout
				switch i2.ChannelCount {
				case 1:
					cl = av.CH_MONO
				case 2:
					cl = av.CH_STEREO
				default:
					cl = av.CH_MONO
				}
				CodecData = codec.NewOpusCodecData(i2.TimeScale, cl)
			case av.PCM_MULAW:
				CodecData = codec.NewPCMMulawCodecData()
			case av.PCM_ALAW:
				CodecData = codec.NewPCMAlawCodecData()
			case av.PCM:
				CodecData = codec.NewPCMCodecData()
			default:
				client.Println("Audio Codec", i2.Type, "not supported")
			}
			if CodecData != nil {
				client.CodecData = append(client.CodecData, CodecData)
				client.audioIDX = int8(len(client.CodecData) - 1)
				client.audioCodec = CodecData.Type()
				if i2.TimeScale != 0 {
					client.AudioTimeScale = int64(i2.TimeScale)
				}
			}
		}
		ch += 2
	}

	err = client.request(PLAY, nil, client.control, false, false, nil)
	if err != nil {
		return nil, err
	}

	for _, media := range client.mediaSDP {
		if (media.AVType != VIDEO && media.AVType != AUDIO) || (client.options.DisableAudio && media.AVType == AUDIO) {
			continue
		}
		//rtp协议走 UDP协议
		if media.MediaProtocol == "RTP/AVP/UDP" {
			ports := strings.Split(media.ServerPort, "-")
			clientPorts := strings.Split(media.ClientPort, "-")
			//启动UDP连接
			splitRemoteHost, _, _ := net.SplitHostPort(client.pURL.Host)
			localRTPAddr, _ := net.ResolveUDPAddr("udp", ":"+clientPorts[0])
			remoteRTPAddr, _ := net.ResolveUDPAddr("udp", net.JoinHostPort(splitRemoteHost, ports[0]))
			rtpConn, err := net.DialUDP("udp", localRTPAddr, remoteRTPAddr)
			if nil != err {
				return nil, err
			}
			localRTCPAddr, _ := net.ResolveUDPAddr("udp", ":"+clientPorts[1])
			remoteRTCPAddr, _ := net.ResolveUDPAddr("udp", net.JoinHostPort(splitRemoteHost, ports[1]))
			rtcpConn, err := net.DialUDP("udp", localRTCPAddr, remoteRTCPAddr)
			if nil != err {
				return nil, err
			}

			go client.startRTPStream(rtpConn)
			go client.startRTCPStream(rtcpConn)
		} else {
			go client.startStream()
		}
	}
	return client, nil
}

func (client *RTSPClient) startRTCPStream(udpConn *net.UDPConn) {
	defer func() {
		client.Signals <- SignalStreamRTPStop
		udpConn.Close()
	}()
	rtcpBuff := make([]byte, 1024)

	for {
		err := udpConn.SetDeadline(time.Now().Add(client.options.ReadWriteTimeout))
		if err != nil {
			client.Println("RTSP Client RTP SetDeadline", err)
			return
		}

		l, _, err := udpConn.ReadFromUDP(rtcpBuff)
		if err != nil && l < RTPHeaderSize {
			client.Println("RTSP Client RTP ReadFromUDP", err)
			return
		}
		//解析rtcp
		_, err = rtcp.Unmarshal(rtcpBuff[:l])
		if nil != err {
			client.Println("RTSP Client RTCP Unmarshal", err)
			return
		}

	}
}

func (client *RTSPClient) startRTPStream(udpConn *net.UDPConn) {
	defer func() {
		client.Signals <- SignalStreamRTPStop
		udpConn.Close()
	}()

	decoder := rtph264.NewDecoder(udpConn)
	timer := time.Now()
	for {
		err := udpConn.SetDeadline(time.Now().Add(client.options.ReadWriteTimeout))
		if err != nil {
			client.Println("RTSP Client RTP SetDeadline", err)
			return
		}
		if int(time.Now().Sub(timer).Seconds()) > 25 {
			err := client.request(OPTIONS, map[string]string{"Require": "implicit-play"}, client.control, false, true, nil)
			if err != nil {
				client.Println("RTSP Client RTP keep-alive", err)
				return
			}
			timer = time.Now()
		}

		//h264报文
		header, nalus, err := decoder.Read()
		if nil != err {
			client.Println("RTSP Client Read " + err.Error())
			return
		}

		for _, nalu := range nalus {
			naluType := nalu[0] & 0x1f
			pkt := &av.Packet{
				Idx:  client.videoIDX,
				Time: time.Duration(header.Timestamp) * time.Second / time.Duration(client.mediaSDP[client.videoIDX].TimeScale),
			}

			// 拼接数据包
			var buf bytes.Buffer
			switch naluType {
			case 1, 2, 3, 4, 5:
				if naluType == 5 {
					pkt.IsKeyFrame = true
				}
				b := make([]byte, 4)
				pio.PutU32BE(b[0:4], uint32(len(nalu)))
				buf.Write(b)
				buf.Write(nalu)
			case h264parser.NALU_SPS:
				//pps
				if len(client.sps) <= 0 || bytes.Compare(client.sps, nalu) != 0 {
					client.sps = nalu
					client.CodecUpdateSPS(nalu)
				}
				buf.Write(nalu)
			case h264parser.NALU_PPS:
				//pps
				if len(client.pps) <= 0 || bytes.Compare(client.pps, nalu) != 0 {
					client.pps = nalu
					client.CodecUpdatePPS(nalu)
				}
				buf.Write(nalu)
			}
			pkt.Data = buf.Bytes()
			client.OutgoingPacketQueue <- pkt
		}

	}
}

func (client *RTSPClient) ControlTrack(track string) string {
	if strings.Contains(track, "rtsp://") {
		return track
	}
	if !strings.HasSuffix(client.control, "/") {
		track = "/" + track
	}
	return client.control + track
}

func (client *RTSPClient) startStream() {
	defer func() {
		client.Signals <- SignalStreamRTPStop
	}()
	timer := time.Now()
	oneb := make([]byte, 1)
	header := make([]byte, 4)
	var fixed bool
	for {
		err := client.conn.SetDeadline(time.Now().Add(client.options.ReadWriteTimeout))
		if err != nil {
			client.Println("RTSP Client RTP SetDeadline", err)
			return
		}
		if int(time.Now().Sub(timer).Seconds()) > 25 {
			err := client.request(OPTIONS, map[string]string{"Require": "implicit-play"}, client.control, false, true, nil)
			if err != nil {
				client.Println("RTSP Client RTP keep-alive", err)
				return
			}
			timer = time.Now()
		}
		if !fixed {
			nb, err := io.ReadFull(client.connRW, header)
			if err != nil || nb != 4 {
				client.Println("RTSP Client RTP Read Header", err)
				return
			}
		}
		fixed = false
		switch header[0] {
		case 0x24:
			length := int32(binary.BigEndian.Uint16(header[2:]))
			if length > 65535 || length < 12 {
				client.Println("RTSP Client RTP Incorrect Packet Size")
				return
			}
			content := make([]byte, length+4)
			//类型
			content[0] = header[0]
			//chid
			content[1] = header[1]
			//长度
			content[2] = header[2]
			content[3] = header[3]
			n, rerr := io.ReadFull(client.connRW, content[4:length+4])
			if rerr != nil || n != int(length) {
				client.Println("RTSP Client RTP ReadFull", err)
				return
			}

			//atomic.AddInt64(&client.Bitrate, int64(length+4))
			if client.options.OutgoingProxy {
				if len(client.OutgoingProxyQueue) < 2000 {
					client.OutgoingProxyQueue <- &content
				} else {
					client.Println("RTSP Client OutgoingProxy Chanel Full")
					return
				}
			}
			pkt, got := client.RTPDemuxer(&content)
			if !got {
				continue
			}
			for i := range pkt {
				client.OutgoingPacketQueue <- pkt[i]
			}
		case 0x52:
			var responseTmp []byte
			for {
				n, rerr := io.ReadFull(client.connRW, oneb)
				if rerr != nil || n != 1 {
					client.Println("RTSP Client RTP Read Keep-Alive Header", rerr)
					return
				}
				responseTmp = append(responseTmp, oneb...)
				if (len(responseTmp) > 4 && bytes.Compare(responseTmp[len(responseTmp)-4:], []byte("\r\n\r\n")) == 0) || len(responseTmp) > 768 {
					if strings.Contains(string(responseTmp), "Content-Length:") {
						si, err := strconv.Atoi(stringInBetween(string(responseTmp), "Content-Length: ", "\r\n"))
						if err != nil {
							client.Println("RTSP Client RTP Read Keep-Alive Content-Length", err)
							return
						}
						cont := make([]byte, si)
						_, err = io.ReadFull(client.connRW, cont)
						if err != nil {
							client.Println("RTSP Client RTP Read Keep-Alive ReadFull", err)
							return
						}
					}
					break
				}
			}
		default:
			client.Println("RTSP Client RTP Read DeSync")
			return
		}
	}
}

func (client *RTSPClient) request(method string, customHeaders map[string]string, uri string, one bool, nores bool, media *sdp.Media) (err error) {
	err = client.conn.SetDeadline(time.Now().Add(client.options.ReadWriteTimeout))
	if err != nil {
		return
	}
	client.seq++
	builder := bytes.Buffer{}
	builder.WriteString(fmt.Sprintf("%s %s RTSP/1.0\r\n", method, uri))
	builder.WriteString(fmt.Sprintf("CSeq: %d\r\n", client.seq))
	if client.clientDigest {
		builder.WriteString(fmt.Sprintf("Authorization: %s\r\n", client.createDigest(method, uri)))
	}
	if customHeaders != nil {
		for k, v := range customHeaders {
			builder.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
		}
	}
	for k, v := range client.headers {
		builder.WriteString(fmt.Sprintf("%s: %s\r\n", k, v))
	}
	builder.WriteString(fmt.Sprintf("\r\n"))
	client.Println(builder.String())
	s := builder.String()
	_, err = client.connRW.WriteString(s)
	if err != nil {
		return
	}
	err = client.connRW.Flush()
	if err != nil {
		return
	}
	builder.Reset()
	if !nores {
		var isPrefix bool
		var line []byte
		var contentLen int
		res := make(map[string]string)
		for {
			line, isPrefix, err = client.connRW.ReadLine()
			if err != nil {
				return
			}
			if strings.Contains(string(line), "RTSP/1.0") && (!strings.Contains(string(line), "200") && !strings.Contains(string(line), "401")) {
				time.Sleep(1 * time.Second)
				err = errors.New("Camera send status" + string(line))
				return
			}
			builder.Write(line)
			if !isPrefix {
				builder.WriteString("\r\n")
			}
			if len(line) == 0 {
				break
			}
			splits := strings.SplitN(string(line), ":", 2)
			if len(splits) == 2 {
				if splits[0] == "Content-length" {
					splits[0] = "Content-Length"
				}
				res[splits[0]] = splits[1]
			}
		}
		if val, ok := res["WWW-Authenticate"]; ok {
			if strings.Contains(val, "Digest") {
				client.realm = stringInBetween(val, "realm=\"", "\"")
				client.nonce = stringInBetween(val, "nonce=\"", "\"")
				client.clientDigest = true
			} else if strings.Contains(val, "Basic") {
				client.headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(client.username+":"+client.password))
				client.clientBasic = true
			}
			if !one {
				err = client.request(method, customHeaders, uri, true, false, nil)
				return
			}
			err = errors.New("RTSP Client Unauthorized 401")
			return
		}
		if val, ok := res["Session"]; ok {
			splits2 := strings.Split(val, ";")
			client.session = strings.TrimSpace(splits2[0])
			client.headers["Session"] = strings.TrimSpace(splits2[0])
		}
		if val, ok := res["Content-Base"]; ok {
			client.control = strings.TrimSpace(val)
		}
		if val, ok := res["RTP-Info"]; ok {
			splits := strings.Split(val, ",")
			for _, v := range splits {
				splits2 := strings.Split(v, ";")
				for _, vs := range splits2 {
					if strings.Contains(vs, "rtptime") {
						splits3 := strings.Split(vs, "=")
						if len(splits3) == 2 {
							if client.startVideoTS == 0 {
								ts, _ := strconv.Atoi(strings.TrimSpace(splits3[1]))
								client.startVideoTS = int64(ts)
							} else {
								ts, _ := strconv.Atoi(strings.TrimSpace(splits3[1]))
								client.startAudioTS = int64(ts)
							}
						}
					}
				}
			}
		}

		if method == DESCRIBE {
			if val, ok := res["Content-Length"]; ok {
				contentLen, err = strconv.Atoi(strings.TrimSpace(val))
				if err != nil {
					return
				}
				client.SDPRaw = make([]byte, contentLen)
				_, err = io.ReadFull(client.connRW, client.SDPRaw)
				if err != nil {
					return
				}
				builder.Write(client.SDPRaw)
				_, client.mediaSDP = sdp.Parse(string(client.SDPRaw))
			}
		}

		//setup阶段获取transport
		if method == SETUP {
			//获取transport
			if trans, ok := res["Transport"]; ok {
				splits := strings.Split(trans, ";")
				for _, item := range splits {
					if strings.Contains(item, "=") {
						kv := strings.SplitN(item, "=", 2)
						if "server_port" == kv[0] {
							if nil != media {
								//获取到端口
								media.ServerPort = kv[1]
							}
						}
					}
				}
			}
		}
		client.Println(builder.String())
	}
	return
}

func (client *RTSPClient) Close() {
	if client.conn != nil {
		client.conn.SetDeadline(time.Now().Add(time.Second))
		client.request(TEARDOWN, nil, client.control, false, true, nil)
		err := client.conn.Close()
		client.Println("RTSP Client Close", err)
	}
}

func (client *RTSPClient) parseURL(rawURL string) error {
	l, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	username := l.User.Username()
	password, _ := l.User.Password()
	l.User = nil
	if l.Port() == "" {
		l.Host = fmt.Sprintf("%s:%s", l.Host, "554")
	}
	if l.Scheme != "rtsp" {
		l.Scheme = "rtsp"
	}
	client.pURL = l
	client.username = username
	client.password = password
	client.control = l.String()
	return nil
}

func (client *RTSPClient) createDigest(method string, uri string) string {
	md5UserRealmPwd := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", client.username, client.realm, client.password))))
	md5MethodURL := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s", method, uri))))
	response := fmt.Sprintf("%x", md5.Sum([]byte(fmt.Sprintf("%s:%s:%s", md5UserRealmPwd, client.nonce, md5MethodURL))))
	Authorization := fmt.Sprintf("Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%s\"", client.username, client.realm, client.nonce, uri, response)
	return Authorization
}

func stringInBetween(str string, start string, end string) (result string) {
	s := strings.Index(str, start)
	if s == -1 {
		return
	}
	str = str[s+len(start):]
	e := strings.Index(str, end)
	if e == -1 {
		return
	}
	str = str[:e]
	return str
}

//var f, _ = os.OpenFile("./a.h264", os.O_CREATE|os.O_APPEND|os.O_RDWR, os.ModePerm)
func (client *RTSPClient) RTPDemuxer(payloadRAW *[]byte) ([]*av.Packet, bool) {
	content := *payloadRAW
	//firstByte := content[4]
	//padding := (firstByte>>5)&1 == 1
	//extension := (firstByte>>4)&1 == 1
	//CSRCCnt := int(firstByte & 0x0f)
	//SequenceNumber := int(binary.BigEndian.Uint16(content[6:8]))
	//timestamp := int64(binary.BigEndian.Uint32(content[8:12]))
	//offset := RTPHeaderSize
	//
	//end := len(content)
	//if end-offset >= 4*CSRCCnt {
	//	offset += 4 * CSRCCnt
	//}
	//if extension && len(content) < 4+offset+2+2 {
	//	return nil, false
	//}
	//if extension && end-offset >= 4 {
	//	extLen := 4 * int(binary.BigEndian.Uint16(content[4+offset+2:]))
	//	offset += 4
	//	if end-offset >= extLen {
	//		offset += extLen
	//	}
	//}
	//if padding && end-offset > 0 {pay
	//	paddingLen := int(content[end-1])
	//	if end-offset >= paddingLen {
	//		end -= paddingLen
	//	}
	//}

	rtpPacket := rtp.Packet{}
	err := rtpPacket.Unmarshal(content[4:])
	if nil != err {
		client.Println("rtp Unmarshal ", err)
		return nil, false
	}

	//offset += 4
	switch int(content[1]) {
	case client.videoID:
		if client.PreVideoTS == 0 {
			client.PreVideoTS = int64(rtpPacket.Timestamp)
		}
		if client.PreSequenceNumber != 0 && int(rtpPacket.SequenceNumber)-client.PreSequenceNumber != 1 {
			client.Println("drop packet", rtpPacket.SequenceNumber-1)
		}
		client.PreSequenceNumber = int(rtpPacket.SequenceNumber)
		if client.BufferRtpPacket.Len() > 4048576 {
			client.Println("Big Buffer Flush")
			client.BufferRtpPacket.Truncate(0)
			client.BufferRtpPacket.Reset()
		}

		if client.videoCodec != av.H264 {
			client.Println("Unsupport Not H264 Format")
			return nil, false
		}

		rtpPayload := rtpPacket.Payload
		//FU-A分片类型
		fuIndicator := rtpPayload[0]
		fuHeader := rtpPayload[1]
		naluType := fuIndicator & 0x1f //获取FU indicator的类型域
		//获取FU header的前三位，判断当前是分包的开始、中间或结束
		flag := fuIndicator & 0xe0
		//nal Fua
		nalFua := flag | (fuHeader & 0x1f)

		var retmap []*av.Packet
		switch {
		//单一分片的NALU
		case naluType >= 1 && naluType <= 5:
			//f.Write(append([]byte{0x00,0x00,0x01},rtpPayload...))
			retmap = append(retmap, &av.Packet{
				Data:            append(binSize(len(rtpPayload)), rtpPayload...),
				CompositionTime: time.Duration(1) * time.Millisecond,
				Idx:             client.videoIDX,
				IsKeyFrame:      naluType == 5,
				Duration:        time.Duration(float32(int64(rtpPacket.Timestamp)-client.PreVideoTS)/90) * time.Millisecond,
				Time:            time.Duration(rtpPacket.Timestamp/90) * time.Millisecond,
			})

		case naluType == 7:
			client.CodecUpdateSPS(rtpPayload)
		case naluType == 8:
			client.CodecUpdatePPS(rtpPayload)
		case naluType == 24:
			client.Println("24 Type need add next version report https://github.com/blackbeans/vdk")
		case naluType == 0x1c:
			//FU-A分片类型
			//FU-A的起始
			isStart := fuHeader&0x80 != 0
			//FU-A的结尾
			isEnd := fuHeader&0x40 != 0
			if isStart {
				client.fuStarted = true
				client.BufferRtpPacket.Truncate(0)
				client.BufferRtpPacket.Reset()
				client.BufferRtpPacket.Write([]byte{nalFua})
			}
			if client.fuStarted {
				client.BufferRtpPacket.Write(rtpPayload[2:])
				if isEnd {
					//中间或者是结束的包那么写入
					client.fuStarted = false
					naluTypef := client.BufferRtpPacket.Bytes()[0] & 0x1f
					if naluTypef == 7 || naluTypef == 9 {
						bufered, _ := h264parser.SplitNALUs(append([]byte{0x00, 0x00, 0x00, 0x01}, client.BufferRtpPacket.Bytes()...))
						for _, v := range bufered {
							naluTypefs := v[0] & 0x1f
							switch {
							case naluTypefs == 5:
								//client.BufferRtpPacket.Reset()
								//client.BufferRtpPacket.Write(v)
								naluTypef = 5
							case naluTypefs == 7:
								client.CodecUpdateSPS(v)
							case naluTypefs == 8:
								client.CodecUpdatePPS(v)
							}
						}
					}

					//写到文件里
					//f.Write(append([]byte{0x00, 0x00, 0x01}, client.BufferRtpPacket.Bytes()...))
					retmap = append(retmap, &av.Packet{
						Data:            append(binSize(client.BufferRtpPacket.Len()), client.BufferRtpPacket.Bytes()...),
						CompositionTime: time.Duration(1) * time.Millisecond,
						Duration:        time.Duration(float32(int64(rtpPacket.Timestamp)-client.PreVideoTS)/90) * time.Millisecond,
						Idx:             client.videoIDX,
						IsKeyFrame:      naluTypef == 5,
						Time:            time.Duration(rtpPacket.Timestamp/90) * time.Millisecond,
					})
				}
			}
		default:
			client.Println("Unsupported NAL Type", naluType)
		}

		if len(retmap) > 0 {
			client.PreVideoTS = int64(rtpPacket.Timestamp)
			return retmap, true
		}
	case client.audioID:
		if client.PreAudioTS == 0 {
			client.PreAudioTS = int64(rtpPacket.Timestamp)
		}
		nalRaw, _ := h264parser.SplitNALUs(rtpPacket.Payload)
		var retmap []*av.Packet
		for _, nal := range nalRaw {
			var duration time.Duration
			switch client.audioCodec {
			case av.PCM_MULAW:
				duration = time.Duration(len(nal)) * time.Second / time.Duration(client.AudioTimeScale)
				client.AudioTimeLine += duration
				retmap = append(retmap, &av.Packet{
					Data:            nal,
					CompositionTime: time.Duration(1) * time.Millisecond,
					Duration:        duration,
					Idx:             client.audioIDX,
					IsKeyFrame:      false,
					Time:            client.AudioTimeLine,
				})
			case av.PCM_ALAW:
				duration = time.Duration(len(nal)) * time.Second / time.Duration(client.AudioTimeScale)
				client.AudioTimeLine += duration
				retmap = append(retmap, &av.Packet{
					Data:            nal,
					CompositionTime: time.Duration(1) * time.Millisecond,
					Duration:        duration,
					Idx:             client.audioIDX,
					IsKeyFrame:      false,
					Time:            client.AudioTimeLine,
				})
			case av.OPUS:
				duration = time.Duration(20) * time.Millisecond
				client.AudioTimeLine += duration
				retmap = append(retmap, &av.Packet{
					Data:            nal,
					CompositionTime: time.Duration(1) * time.Millisecond,
					Duration:        duration,
					Idx:             client.audioIDX,
					IsKeyFrame:      false,
					Time:            client.AudioTimeLine,
				})
			case av.AAC:
				auHeadersLength := uint16(0) | (uint16(nal[0]) << 8) | uint16(nal[1])
				auHeadersCount := auHeadersLength >> 4
				framesPayloadOffset := 2 + int(auHeadersCount)<<1
				auHeaders := nal[2:framesPayloadOffset]
				framesPayload := nal[framesPayloadOffset:]
				for i := 0; i < int(auHeadersCount); i++ {
					auHeader := uint16(0) | (uint16(auHeaders[0]) << 8) | uint16(auHeaders[1])
					frameSize := auHeader >> 3
					frame := framesPayload[:frameSize]
					auHeaders = auHeaders[2:]
					framesPayload = framesPayload[frameSize:]
					if _, _, _, _, err := aacparser.ParseADTSHeader(frame); err == nil {
						frame = frame[7:]
					}
					duration = time.Duration((float32(1024)/float32(client.AudioTimeScale))*1000) * time.Millisecond
					client.AudioTimeLine += duration
					retmap = append(retmap, &av.Packet{
						Data:            frame,
						CompositionTime: time.Duration(1) * time.Millisecond,
						Duration:        duration,
						Idx:             client.audioIDX,
						IsKeyFrame:      false,
						Time:            client.AudioTimeLine,
					})
				}
			}
		}
		if len(retmap) > 0 {
			client.PreAudioTS = int64(rtpPacket.Timestamp)
			return retmap, true
		}
	default:
		//client.Println("Unsuported Intervaled data packet", int(content[1]), content[offset:end])
	}
	return nil, false
}

func (client *RTSPClient) CodecUpdateSPS(val []byte) {
	if client.videoCodec != av.H264 && client.videoCodec != av.H265 {
		return
	}
	if bytes.Compare(val, client.sps) == 0 {
		return
	}
	client.sps = val
	if (client.videoCodec == av.H264 && len(client.pps) == 0) || (client.videoCodec == av.H265 && (len(client.vps) == 0 || len(client.pps) == 0)) {
		return
	}
	var codecData av.VideoCodecData
	var err error
	switch client.videoCodec {
	case av.H264:
		client.Println("Codec Update SPS", val)
		codecData, err = h264parser.NewCodecDataFromSPSAndPPS(val, client.pps)
		if err != nil {
			client.Println("Parse Codec Data Error", err)
			return
		}
	case av.H265:
		codecData, err = h265parser.NewCodecDataFromVPSAndSPSAndPPS(client.vps, val, client.pps)
		if err != nil {
			client.Println("Parse Codec Data Error", err)
			return
		}
	}
	if len(client.CodecData) > 0 {
		for i, i2 := range client.CodecData {
			if i2.Type().IsVideo() {
				client.CodecData[i] = codecData
			}
		}
	} else {
		client.CodecData = append(client.CodecData, codecData)
	}
	client.Signals <- SignalCodecUpdate
}

func (client *RTSPClient) CodecUpdatePPS(val []byte) {
	if client.videoCodec != av.H264 && client.videoCodec != av.H265 {
		return
	}
	if bytes.Compare(val, client.pps) == 0 {
		return
	}
	client.pps = val
	if (client.videoCodec == av.H264 && len(client.sps) == 0) || (client.videoCodec == av.H265 && (len(client.vps) == 0 || len(client.sps) == 0)) {
		return
	}
	var codecData av.VideoCodecData
	var err error
	switch client.videoCodec {
	case av.H264:
		client.Println("Codec Update PPS", val)
		codecData, err = h264parser.NewCodecDataFromSPSAndPPS(client.sps, val)
		if err != nil {
			client.Println("Parse Codec Data Error", err)
			return
		}
	case av.H265:
		codecData, err = h265parser.NewCodecDataFromVPSAndSPSAndPPS(client.vps, client.sps, val)
		if err != nil {
			client.Println("Parse Codec Data Error", err)
			return
		}
	}
	if len(client.CodecData) > 0 {
		for i, i2 := range client.CodecData {
			if i2.Type().IsVideo() {
				client.CodecData[i] = codecData
			}
		}
	} else {
		client.CodecData = append(client.CodecData, codecData)
	}
	client.Signals <- SignalCodecUpdate
}

func (client *RTSPClient) CodecUpdateVPS(val []byte) {
	if client.videoCodec != av.H265 {
		return
	}
	if bytes.Compare(val, client.vps) == 0 {
		return
	}
	client.vps = val
	if len(client.sps) == 0 || len(client.pps) == 0 {
		return
	}
	codecData, err := h265parser.NewCodecDataFromVPSAndSPSAndPPS(val, client.sps, client.pps)
	if err != nil {
		client.Println("Parse Codec Data Error", err)
		return
	}
	if len(client.CodecData) > 0 {
		for i, i2 := range client.CodecData {
			if i2.Type().IsVideo() {
				client.CodecData[i] = codecData
			}
		}
	} else {
		client.CodecData = append(client.CodecData, codecData)
	}
	client.Signals <- SignalCodecUpdate
}

//Println mini logging functions
func (client *RTSPClient) Println(v ...interface{}) {
	if client.options.Debug {
		log.Println(v)
	}
}

//binSize
func binSize(val int) []byte {
	buf := make([]byte, 4)
	binary.BigEndian.PutUint32(buf, uint32(val))
	return buf
}
