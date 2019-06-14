package src

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"log"
	"math/rand"
	"net"
	"strconv"

	"github.com/marksamman/bencode"
)

// Tracker is
type Tracker struct {
	Torr  Torr
	Peers []Peer
}

// Peer ..
type Peer struct {
	IP   net.IP
	Port uint16
}

// ReadTorr reads data from torrent file
func (t *Tracker) ReadTorr(fp string) {
	t.Torr.Read(fp)
}

// ConnUDP sends connection request to announce address and
// returns the relevent response data
func (t *Tracker) ConnUDP(addr string, tid uint32) (uint32, uint64, error) {
	// creating connection request packet, to be sent to the client. It includes,
	// for more details visit http://www.bittorrent.org/beps/bep_0015.html
	// 0-8 -> protocol_id -> 64-bit integer -> 0x41727101980 (magic constant)
	// 8-12 -> action -> 32-bit integer -> 0 (constant, 0 indicates a connection req)
	// 12-16 -> transaction_id -> 32-bit integer -> client identifier
	var el = []interface{}{uint64(0x41727101980), uint32(0), uint32(tid)} // temporarily holds the data in an array

	// writing the data to a buffer, to be send in the request
	buf := new(bytes.Buffer)
	for i, v := range el {
		// appending each element to the buffer
		err := binary.Write(buf, binary.BigEndian, v)
		if err != nil {
			fmt.Println("buffer write failed for connection request build: i =", i)
			return 0, 0, err
		}
	}

	// UDP protocol doesn't esablish any connection between client and server, the
	// connection doesn't actually represents any actual connection in transition layer
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()

	// writing the data to the server
	nw, err := conn.Write(buf.Bytes())
	if err != nil {
		return 0, 0, err
	}
	fmt.Printf("written %v bytes to as udp connection request\n", nw)

	// reading the connection response recieved from the server,
	// it includes some data that requered for the announce request
	// 0-4 -> action -> 32-bit integer -> 0 (indicates a connection req)
	// 4-8 -> transaction_id -> 32-bit integer -> same as sent in the request
	// 8-16 -> connection_id -> 64-bit integer -> connection id
	resp := make([]byte, 16)
	nr, err := bufio.NewReader(conn).Read(resp)
	if err != nil {
		return 0, 0, err
	}
	fmt.Printf("read %v bytes from udp connection response\n", nr)

	// error check, otherwise len(resp) is less than 16 bytes,
	// it world fail to extract data from it
	if len(resp) < 16 {
		fmt.Printf("the response length is shorter then 16 bytes")
		return 0, 0, err
	}

	// returning as the actual types
	// TODO: returning as []byte, would be easier for resending the data (ex: connection_id)
	BE := binary.BigEndian
	return BE.Uint32(resp[4:12]), BE.Uint64(resp[8:16]), err
}

// AnnounceUDP ...
func (t *Tracker) AnnounceUDP(addr string, tid uint32, cid uint64) (uint32, error) {
	numseed := 20 // number of requested seeders

	// building buffer to be sent with the announce request
	buf, err := announceDataUDP(&t.Torr, tid, cid, numseed)
	if err != nil {
		return 0, err
	}

	// udp dial (IK, just because comments are sexy)
	conn, err := net.Dial("udp", addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	// writing to data to the udp server
	nw, err := conn.Write(buf.Bytes())
	if err != nil {
		return 0, err
	}
	fmt.Printf("written %v bytes to as udp announce request\n", nw)

	// reading tracker response
	resp := make([]byte, 20+numseed*6) // 20+numseed*6 is the largest possible value for response (cause numseed is finite)
	nr, err := bufio.NewReader(conn).Read(resp)
	if err != nil {
		return 0, err
	}

	fmt.Printf("read %v bytes from udp announce response\n", nr)
	resp = resp[:nr] // skipping rest of the bytes, only populated ones contains all the data

	// if len(resp) < 20, somethings wrong with the response
	if len(resp) < 20 {
		fmt.Printf("the response length is shorter than 20 bytes")
		return 0, err
	}

	// extracting data from response, the response is formatted like,
	// 0-4 -> 32-bit integer -> action -> 1 (announce), not needed now
	BE := binary.BigEndian

	// 4-8 -> 32-bit integer -> transaction_id
	transactionID := BE.Uint32(resp[4:8])
	if tid != transactionID {
		fmt.Printf("transaction_id did not match, %v != %v\n", tid, transactionID)
		return 0, err
	}

	interval := BE.Uint32(resp[8:12]) // 8-12 -> 32-bit integer -> interval (new announce req can not be made until interval seconds have passed)
	// leechers := BE.Uint32(resp[12:16]) // 12-16 -> 32-bit integer -> leechers
	seeders := BE.Uint32(resp[16:20]) // 16-20 -> 32-bit integer -> number of seeders

	fmt.Printf("announce response recieved, transaction_id = %v\n", transactionID)
	fmt.Printf("number of seeders found = %v *****\n", seeders)

	// 20-nr -> rest of the part contains peer (seeder) information, 6 bytes for each peer
	// first 4 bytes are IP address and last 2 bytes are port. Reading the peer info,
	// more about it http://www.bittorrent.org/beps/bep_0015.html
	i := 20 // for 21st byte
	for {
		if i >= len(resp) {
			// if end of resp data the break
			break
		}
		// reading peer info and appending it the the tracker struct,
		peer := Peer{IP: net.IP(resp[i : i+4]), Port: binary.BigEndian.Uint16(resp[i+4 : i+6])}
		t.Peers = append(t.Peers, peer)
		i += 6
	}

	fmt.Printf("%+v\n", t.Peers)

	return interval, nil
}

// func announceRespUDP()

// announceDataUDP takes a *Torr and returns a formatted buffer
// that contains required elements for UDP announce requests
func announceDataUDP(torr *Torr, tid uint32, cid uint64, numseed int) (*bytes.Buffer, error) {
	// calculating total number of bytes left to be downloaded at start, from torr["info"] values
	var totalbytes int // total number of bytes

	if info, ok := (*torr)["info"].(map[string]interface{}); ok {
		var pl int // pl holds the value of `piece length`, length of each piece in bytes (its equal for all pieces)

		// iretating over info and reading necessary fields
		for k, v := range info {
			switch k {
			case "piece length":
				pl = int(v.(int64)) // each piece length
			case "pieces":
				// `pieces` contains of hashed values for all files, each hash is 20 bytes long,
				// so deviding the length of `pieces's value` will give us the number of pieces,
				// and multiplying it with `piece length` will be the total size of all pieces
				totalbytes = pl * (len([]byte(v.(string))) / 20)
			}
		}
	}

	// constructing buffer for required for request packet,
	// for more details visit http://www.bittorrent.org/beps/bep_0015.html

	// to temporarily hold the data in an array
	var el = []interface{}{
		uint64(cid),                            // 0-8 -> connection_id -> connection_id recieved from connection response
		uint32(1),                              // 8-12 -> action -> 1, represents announce request
		uint32(tid),                            // 12-16 -> transaction_id -> transaction_id from conn-response
		infohash((*torr)["info"]),              // 16-36 -> info_hash -> sha1 hash of encoded (bencode) info_hash property of torr metadata
		genpeerid(),                            // 36-56 -> peer_id -> used as a unique ID for the client, generated by the client at startup
		uint64(0),                              // 56-64 -> downloaded -> how much has been downloaded (0 at start)
		uint64(totalbytes),                     // 64-72 -> left -> how many bytes are yet to be downloaded
		uint64(0),                              // 72-80 -> uploaded -> how much has been uploaded
		uint32(2),                              // 80-84 -> event -> 2 (0: none; 1: completed; 2: started; 3: stopped)
		binary.BigEndian.Uint32(getclientip()), // 84-88 -> IP -> client's ip address
		uint32(0),                              // 88-92 -> key -> for identification (optional)
		uint32(numseed),                        // 92-96 -> num_want -> -1 is default (number of peers that the client would like to receive)
		uint32(6888),                           // 96-98 -> port -> port that the client is listening on (typically 6881-6889
	}

	// writing the data to a buffer, to be send in the request
	buf := new(bytes.Buffer)
	for i, v := range el {
		// appending each element to the buffer
		err := binary.Write(buf, binary.BigEndian, v)
		if err != nil {
			fmt.Println("buffer write failed for announce request build: i =", i)
			return buf, err
		}
	}

	return buf, nil
}

// getclientip returns local machines primary IP-address
func getclientip() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	return conn.LocalAddr().(*net.UDPAddr).IP
}

// infohash gets value of info from metainfo map, and
// returns a 20 byte long sha1 hash of all the info contents
func infohash(info interface{}) []byte {
	enc := bencode.Encode(info)
	h := sha1.New()
	h.Write(enc)
	hash := h.Sum(nil)

	return hash
}

// genpeerid generates a somewhat random peerid (not best solution)
func genpeerid() []byte {
	// return "-qB3130-kwsSnUYwydys"

	var r string
	for i := 0; i < 12; i++ {
		r += strconv.Itoa(rand.Intn(9))
	}
	return []byte("-TC0001-" + r)[:20]
}