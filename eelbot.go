package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

const PROTOCOL18 = 47

func main() {
	fmt.Println("*** eelbot by TeapotDev (Minecraft 1.8.x) ***")
	proxy := flag.String("proxy", "127.0.0.1:25588", "proxy address (client is connecting to it)")
	target := flag.String("target", "127.0.0.1:25565", "target server address")
	count := flag.Int("count", 10, "amount of bots to be connected")
	joindelay := flag.Int("joind", 0, "timeout between bot joins in milliseconds")
	errdelay := flag.Int("errd", 4100, "timeout in milliseconds if client was kicked while connecting")
	eeldelay := flag.Int("eeld", 100, "timeout between bots' actions (snake effect)")
	keepConn := flag.Bool("keep", false, "keeps connections after main client disconnects")
	flag.Parse()

	listener, err := net.Listen("tcp", *proxy)
	if err != nil {
		fmt.Println("## Error starting proxy:", err.Error())
		os.Exit(1)
	}

	var conn net.Conn
	var reader *bufio.Reader
	var writer *bufio.Writer
	var protocolVersion int32
	packetbuf := new(bytes.Buffer)
	fmt.Println("## Waiting for connection to proxy...")
	for {
		conn, err = listener.Accept()
		if err != nil {
			fmt.Println("# Error accepting client:", err.Error())
			conn.Close()
			continue
		}
		reader = bufio.NewReader(conn)
		writer = bufio.NewWriter(conn)

		// C->S 0x00 Handshake
		id, err := readHeader(reader, -1)
		if err != nil {
			fmt.Println("# Error receiving from client:", err.Error())
			conn.Close()
			continue
		}
		if id != 0x00 {
			fmt.Println("# Error receiving from client: unexpected packet")
			conn.Close()
			continue
		}
		protocolVersion, _ = readVarInt(reader)
		readVarString(reader, 64)
		readShort(reader)
		if next, err := readVarInt(reader); next != 2 {
			if err != nil {
				fmt.Println("# Error receiving from client:", err.Error())
				conn.Close()
				continue
			}
			fmt.Println("# Status query!")
			go handleStatus(conn, reader, writer, protocolVersion, *count)
			continue
		}

		// C->S Login start
		id, err = readHeader(reader, -1)
		if err != nil {
			fmt.Println("# Error receiving from client:", err.Error())
			conn.Close()
			continue
		}
		if id != 0x00 {
			fmt.Println("# Error receiving from client: unexpected packet")
			conn.Close()
			continue
		}
		readVarString(reader, 64)
		break
	}

	keepAliveStop := make(chan int)
	keepAliveStopped := make(chan int)

	var firstReader *bufio.Reader // one of bots is client (main bot)
	otherWriters := make([]*bufio.Writer, *count)

	mutexes := make([]*sync.Mutex, len(otherWriters))
	for i := 0; i < len(mutexes); i++ {
		mutexes[i] = new(sync.Mutex)
	}

	var serverThreshold int32 = -1
	fmt.Println("## Client connected to proxy! Connecting eel to target...")
	for i := 0; i < *count; i++ {
		nick := fmt.Sprintf("makineelbot%d", i)
		fmt.Printf("# Connecting %d: %s\n", i+1, nick)
		other, err := net.Dial("tcp", *target)
		if err != nil {
			fmt.Printf("# Error connecting %d: %s\n", i+1, err.Error())
			time.Sleep(time.Duration(*errdelay) * time.Millisecond)
			i--
			continue
		}
		otherWriter := bufio.NewWriter(other)

		// C->S Handshake
		writeVarInt(packetbuf, 0x00)
		writeVarInt(packetbuf, protocolVersion)
		writeVarString(packetbuf, "minecraft.net")
		writeShort(packetbuf, 25565)
		writeVarInt(packetbuf, 2)
		if err = writePacketBuf(otherWriter, packetbuf, -1); err != nil {
			fmt.Printf("# Error connecting %d: %s\n", i+1, err.Error())
			time.Sleep(time.Duration(*errdelay) * time.Millisecond)
			other.Close()
			i--
			continue
		}

		// C->S Login start
		writeVarInt(packetbuf, 0x00)
		writeVarString(packetbuf, nick)
		if err = writePacketBuf(otherWriter, packetbuf, -1); err != nil {
			fmt.Printf("# Error connecting %d: %s\n", i+1, err.Error())
			time.Sleep(time.Duration(*errdelay) * time.Millisecond)
			other.Close()
			i--
			continue
		}

		otherReader := bufio.NewReader(other)

		var botThreshold int32 = -1

		var handleLogin func() bool
		handleLogin = func() bool {
			// S->C Login success
			id, err := readHeader(otherReader, botThreshold)
			if err != nil {
				fmt.Printf("# Error connecting %d: %s\n", i+1, err.Error())
				time.Sleep(time.Duration(*errdelay) * time.Millisecond)
				other.Close()
				i--
				return false
			}
			switch id {
			// login success
			case 0x02:
				readVarString(otherReader, 36) // uuid
				readVarString(otherReader, 16) // username
				return true
			// disconnect
			case 0x00:
				reason, err := readVarString(otherReader, 512)
				if err == nil {
					fmt.Printf("# Error connecting %d: kicked: %s\n", i+1, reason)
				} else {
					fmt.Printf("# Error connecting %d: %s\n", i+1, err.Error())
				}
				return false
			// encryption request
			case 0x01:
				fmt.Printf("# Error connecting %d: online mode not supported\n", i+1)
				return false
			// set compression
			case 0x03:
				botThreshold, err = readVarInt(otherReader)
				if err != nil {
					fmt.Printf("Error connecting %d: could not read compression threshold: %s\n", i+1, err)
				} else {
					if firstReader == nil {
						fmt.Printf("# Connection threshold: %d\n", botThreshold)

						// set compression for our main client
						// proxy -> main client - set compression (login state)
						writeVarInt(packetbuf, 0x03)
						writeVarInt(packetbuf, botThreshold)
						if err = writePacketBuf(writer, packetbuf, -1); err != nil {
							fmt.Printf("# Error writing setcompression %d: %s\n", i+1, err.Error())
							os.Exit(2)
						}
					}
					// try to get login success yet again
					return handleLogin()
				}
			default:
				return false
			}
			return false
		}

		if !handleLogin() {
			time.Sleep(time.Duration(*errdelay) * time.Millisecond)
			other.Close()
			i--
			continue
		}

		if firstReader == nil {
			serverThreshold = botThreshold
			firstReader = otherReader

			// S->C Login success
			writeVarInt(packetbuf, 0x02)
			writeVarString(packetbuf, "069a79f4-44e9-4726-a5be-fca90e38aaf5")
			writeVarString(packetbuf, "eel")
			if err = writePacketBuf(writer, packetbuf, serverThreshold); err != nil {
				fmt.Println("## Error sending to client: " + err.Error())
				os.Exit(1)
			}

			go keepAlive(writer, keepAliveStop, keepAliveStopped, true, serverThreshold, nil)
		} else {
			go dummyRead(otherReader)
		}
		otherWriters[i] = otherWriter
		go keepAlive(otherWriter, nil, keepAliveStopped, false, serverThreshold, mutexes[i])
		time.Sleep(time.Duration(*joindelay) * time.Millisecond)
	}

	firstWriter := otherWriters[0] // main bot
	otherWriters = otherWriters[1:]

	// stopping async keep alive
	keepAliveStop <- 1
	<-keepAliveStopped

	go func() {
		var buffer [2048]byte
		for {
			// read from main bot stream and write to client
			count, err := firstReader.Read(buffer[:])
			if err != nil {
				fmt.Println("# Error reading from main connection:", err.Error())
				break
			}
			_, err = writer.Write(buffer[:count])
			if err != nil {
				fmt.Println("# Error writing to main connection:", err.Error())
				break
			}
			err = writer.Flush()
			if err != nil {
				fmt.Println("# Error writing to main connection:", err.Error())
				break
			}
		}

		if !(*keepConn) {
			os.Exit(0)
		}
	}()

	eelDuration := time.Duration(*eeldelay) * time.Millisecond
	writerChannels := make([]chan redirectedPacket, len(otherWriters))
	for i := range writerChannels {
		c := make(chan redirectedPacket, len(otherWriters)*(60*(*eeldelay/50+1)))
		writerChannels[i] = c

		icopy := i
		otherWriter := otherWriters[i]
		go func() {
			for packet := range c {
				diff := packet.runAt.Sub(time.Now())
				if diff > 0 {
					time.Sleep(diff)
				}

				mutexes[icopy+1].Lock()
				writeVarInt(otherWriter, packet.length)
				otherWriter.Write(packet.data)
				otherWriter.Flush()
				mutexes[icopy+1].Unlock()
			}
		}()
	}

	fmt.Println("## Redirecting!")
	for {
		// reading packet from client
		length, err := readVarInt(reader)
		if err != nil {
			fmt.Println("# Error reading from main connection:", err.Error())
			break
		}
		buffer := make([]byte, length)
		_, err = io.ReadFull(reader, buffer)
		if err != nil {
			fmt.Println("# Error reading from main connection:", err.Error())
			break
		}

		// writing packet to main bot
		mutexes[0].Lock()
		writeVarInt(firstWriter, length)
		firstWriter.Write(buffer)
		firstWriter.Flush()
		mutexes[0].Unlock()

		// writing packet to other bots with timeout
		now := time.Now()
		for i, c := range writerChannels {
			c <- redirectedPacket{runAt: now.Add((time.Duration(i+1) * eelDuration)), length: length, data: buffer}
		}
	}

	if *keepConn {
		for j := int32(0); ; j++ {
			time.Sleep(5 * time.Second)

			writeKeepAlive(firstWriter, serverThreshold)
			firstWriter.Flush()

			for i, otherWriter := range otherWriters {
				mutexes[i].Lock()
				writeKeepAlive(otherWriter, serverThreshold)
				otherWriter.Flush()
				mutexes[i].Unlock()
			}
		}
	}
}

type redirectedPacket struct {
	runAt  time.Time
	length int32
	data   []byte
}

func keepAlive(writer *bufio.Writer, stop, stopped chan int, quitOnErr bool, threshold int32, mutex *sync.Mutex) { // used to keep alive while connecting other bots
	ticker := time.NewTicker(3 * time.Second)
	defer func() {
		ticker.Stop()
		if stopped != nil {
			stopped <- 1
		}
	}()
	for {
		select {
		case <-ticker.C:
		case <-stop:
			return
		}

		if mutex != nil {
			mutex.Lock()
		}

		writeKeepAlive(writer, threshold)
		if err := writer.Flush(); quitOnErr && err != nil {
			fmt.Println("## Error while sending keep alive: " + err.Error())
			if mutex != nil {
				mutex.Unlock()
			}
			return
		}

		if mutex != nil {
			mutex.Unlock()
		}
	}
}

func writeKeepAlive(writer *bufio.Writer, threshold int32) {
	if threshold >= 0 {
		writeByte(writer, 3) // packet length
		writeByte(writer, 0) // compressed length
	} else {
		writeByte(writer, 2) // packet length
	}
	writeByte(writer, 0) // packet id
	writeByte(writer, 0) // keep alive id
}

func dummyRead(reader io.Reader) {
	var buf [4096]byte
	for {
		_, err := reader.Read(buf[:])
		if err != nil {
			return
		}
	}
}
