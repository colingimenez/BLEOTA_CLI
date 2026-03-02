package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"time"

	"tinygo.org/x/bluetooth"
)

const (
	ServiceUUID  = "00008018-0000-1000-8000-00805f9b34fb"
	CommandUUID  = "00008022-0000-1000-8000-00805f9b34fb"
	FirmwareUUID = "00008020-0000-1000-8000-00805f9b34fb"

	StartOTA    uint16 = 0x0001
	StopOTA     uint16 = 0x0002
	StartSpiffs uint16 = 0x0004

	SectorSize = 4096
)

func compressData(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)

	_, err := w.Write(data)
	if err != nil {
		err := w.Close()
		if err != nil {
			return nil, err
		}
		return nil, err
	}

	err = w.Close()
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

func crc16(init uint16, data []byte) uint16 {
	crc := init
	for _, b := range data {
		crc ^= uint16(b) << 8
		for j := 0; j < 8; j++ {
			if crc&0x8000 != 0 {
				crc = ((crc << 1) ^ 0x1021) & 0xFFFF
			} else {
				crc <<= 1
			}
		}
	}
	return crc & 0xFFFF
}

var adapter = bluetooth.DefaultAdapter

func main() {
	deviceName := flag.String("name", "", "BLE device name")
	deviceAddr := flag.String("address", "", "BLE device address (UUID on macOS)")
	firmwarePath := flag.String("file", "", "Path to firmware file")
	isSpiffs := flag.Bool("spiffs", false, "Upload to SPIFFS instead of app partition")
	compress := flag.Bool("compress", false, "Compress firmware with zlib before uploading")
	flag.Parse()

	if (*deviceName == "" && *deviceAddr == "") || *firmwarePath == "" {
		flag.Usage()
		os.Exit(1)
	}

	data, err := os.ReadFile(*firmwarePath)
	if err != nil {
		log.Fatalf("Failed to read firmware file: %v", err)
	}

	originalSize := len(data)
	if *compress {
		fmt.Printf("Compressing firmware (%d bytes)...\n", originalSize)
		data, err = compressData(data)
		if err != nil {
			log.Fatalf("Failed to compress firmware: %v", err)
		}
		fmt.Printf("Compressed to %d bytes (%.2f%% of original)\n", len(data), float64(len(data))/float64(originalSize)*100)
	}

	if err := adapter.Enable(); err != nil {
		log.Fatalf("Failed to enable Bluetooth adapter: %v", err)
	}

	target := *deviceName
	if target == "" {
		target = *deviceAddr
	}
	fmt.Printf("Searching for device: %s...\n", target)
	address, actualName, err := findDevice(*deviceName, *deviceAddr)
	if err != nil {
		log.Fatalf("Failed to find device: %v", err)
	}

	if actualName != "" {
		target = actualName
	}
	fmt.Printf("Connecting to %s...\n", target)
	device, err := adapter.Connect(address, bluetooth.ConnectionParams{})
	if err != nil {
		log.Fatalf("Failed to connect: %v", err)
	}
	defer func(device bluetooth.Device) {
		err := device.Disconnect()
		if err != nil {
			log.Fatalf("Failed to disconnect: %v", err)
		}
	}(device)

	fmt.Println("Connected. Discovering services...")
	serviceUUID, _ := bluetooth.ParseUUID(ServiceUUID)
	services, err := device.DiscoverServices([]bluetooth.UUID{serviceUUID})
	if err != nil || len(services) == 0 {
		log.Fatalf("Failed to discover OTA service: %v", err)
	}

	service := services[0]
	chars, err := service.DiscoverCharacteristics([]bluetooth.UUID{})
	if err != nil {
		log.Fatalf("Failed to discover characteristics: %v", err)
	}

	var cmdChar, fwChar bluetooth.DeviceCharacteristic
	var cmdFound, fwFound bool
	cmdUUID, _ := bluetooth.ParseUUID(CommandUUID)
	fwUUID, _ := bluetooth.ParseUUID(FirmwareUUID)

	for i := range chars {
		if chars[i].UUID() == cmdUUID {
			cmdChar = chars[i]
			cmdFound = true
		} else if chars[i].UUID() == fwUUID {
			fwChar = chars[i]
			fwFound = true
		}
	}

	if !cmdFound || !fwFound {
		log.Fatal("Could not find all required characteristics")
	}

	ota := &OTAClient{
		device:   device,
		cmdChar:  cmdChar,
		fwChar:   fwChar,
		data:     data,
		isSpiffs: *isSpiffs,
	}

	if err := ota.Run(); err != nil {
		log.Fatalf("OTA failed: %v", err)
	}

	fmt.Println("OTA Update successful!")
}

type OTAClient struct {
	device   bluetooth.Device
	cmdChar  bluetooth.DeviceCharacteristic
	fwChar   bluetooth.DeviceCharacteristic
	data     []byte
	isSpiffs bool

	cmdChan chan []byte
	fwChan  chan []byte

	packetSize int
}

func (o *OTAClient) Run() error {
	o.cmdChan = make(chan []byte, 10)
	o.fwChan = make(chan []byte, 10)

	// Enable notifications
	err := o.cmdChar.EnableNotifications(func(value []byte) {
		select {
		case o.cmdChan <- value:
		default:
		}
	})
	if err != nil {
		return fmt.Errorf("failed to enable cmd notifications: %v", err)
	}

	err = o.fwChar.EnableNotifications(func(value []byte) {
		select {
		case o.fwChan <- value:
		default:
		}
	})
	if err != nil {
		return fmt.Errorf("failed to enable fw notifications: %v", err)
	}

	// 0. Negotiate packet size
	mtu, err := o.fwChar.GetMTU()
	if err == nil && mtu > 25 {
		o.packetSize = int(mtu) - 20
		if o.packetSize > 490 {
			o.packetSize = 490
		}
		fmt.Printf("Negotiated MTU: %d, using packet size: %d\n", mtu, o.packetSize)
	} else {
		o.packetSize = 13 // Very conservative default for BLE (MTU 23 - 3 ATT - 3 Header - 2 CRC - 2 safety)
		if err != nil {
			fmt.Printf("Could not get MTU (%v), using conservative default packet size: %d\n", err, o.packetSize)
		} else {
			fmt.Printf("MTU too small (%d), using conservative default packet size: %d\n", mtu, o.packetSize)
		}
	}

	// 1. Handshake (Start OTA)
	o.flushChannels()
	fmt.Println("Starting OTA...")
	if err := o.startOTA(); err != nil {
		return err
	}

	// 2. Send Firmware
	fmt.Println("Sending firmware...")
	if err := o.sendFirmware(); err != nil {
		return err
	}

	// 3. Stop OTA
	o.flushChannels()
	fmt.Println("Finishing OTA...")
	if err := o.stopOTA(); err != nil {
		return err
	}

	return nil
}

func (o *OTAClient) startOTA() error {
	buf := make([]byte, 20)
	otaCmd := StartOTA
	if o.isSpiffs {
		otaCmd = StartSpiffs
	}
	binary.LittleEndian.PutUint16(buf[0:2], otaCmd)
	binary.LittleEndian.PutUint32(buf[2:6], uint32(len(o.data)))
	// buf[6:18] is 0
	crc := crc16(0, buf[0:18])
	binary.LittleEndian.PutUint16(buf[18:20], crc)

	_, err := o.cmdChar.WriteWithoutResponse(buf)
	if err != nil {
		return err
	}

	return o.waitForCommandACK(otaCmd, "start")
}

func (o *OTAClient) waitForCommandACK(expectedCmd uint16, stage string) error {
	select {
	case resp := <-o.cmdChan:
		if len(resp) < 20 {
			return fmt.Errorf("invalid response length during %s: %d (expected 20)", stage, len(resp))
		}
		// Verify CRC
		expectedCRC := binary.LittleEndian.Uint16(resp[18:20])
		actualCRC := crc16(0, resp[0:18])
		if expectedCRC != actualCRC {
			return fmt.Errorf("CRC error in response during %s: expected %04x, got %04x", stage, expectedCRC, actualCRC)
		}

		cmdID := binary.LittleEndian.Uint16(resp[0:2])
		var status uint16

		if cmdID == 3 {
			// NimBLE protocol: [3, 0, cmdID, 0, status, 0, ...]
			respCmdID := binary.LittleEndian.Uint16(resp[2:4])
			if respCmdID != expectedCmd {
				return fmt.Errorf("unexpected command ID in NimBLE response during %s: expected %d, got %d", stage, expectedCmd, respCmdID)
			}
			status = binary.LittleEndian.Uint16(resp[4:6])
		} else {
			// Standard protocol: [cmdID, 0, status, 0, ...]
			if cmdID != expectedCmd {
				return fmt.Errorf("unexpected command ID in response during %s: expected %d, got %d", stage, expectedCmd, cmdID)
			}
			status = binary.LittleEndian.Uint16(resp[2:4])
		}

		if status != 0 {
			return fmt.Errorf("NACK from device during %s: %d (raw: %x)", stage, status, resp)
		}
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("timeout waiting for %s ACK", stage)
	}
}

func (o *OTAClient) flushChannels() {
	for {
		select {
		case <-o.cmdChan:
		case <-o.fwChan:
		default:
			return
		}
	}
}

func (o *OTAClient) sendFirmware() error {
	totalSize := len(o.data)
	writtenSize := 0
	sectorIndex := uint16(0)
	startTime := time.Now()

	for writtenSize < totalSize {
		end := writtenSize + SectorSize
		if end > totalSize {
			end = totalSize
		}
		sectorData := o.data[writtenSize:end]

		err := o.sendSector(sectorIndex, sectorData)
		if err != nil {
			fmt.Printf("\nError sending sector %d: %v\n", sectorIndex, err)
		}

		if err != nil {
			return err
		}

		writtenSize = end
		sectorIndex++

		progress := float64(writtenSize) / float64(totalSize) * 100
		speed := float64(writtenSize) / 1024 / time.Since(startTime).Seconds()
		fmt.Printf("\rProgress: %.2f%% (%.2f KB/s)", progress, speed)
	}
	fmt.Println()
	return nil
}

func (o *OTAClient) sendSector(index uint16, data []byte) error {
	o.flushChannels()
	sectorCRC := uint16(0)
	sectorSize := len(data)
	offset := 0
	sequence := uint8(0)

	for offset < sectorSize {
		toRead := o.packetSize
		isLastPacket := false
		if offset+toRead >= sectorSize {
			toRead = sectorSize - offset
			isLastPacket = true
		}

		packetData := data[offset : offset+toRead]
		sectorCRC = crc16(sectorCRC, packetData)

		if isLastPacket {
			sequence = 0xFF
		}

		packet := make([]byte, 3+len(packetData))
		binary.LittleEndian.PutUint16(packet[0:2], index)
		packet[2] = sequence
		copy(packet[3:], packetData)

		if isLastPacket {
			crcBuf := make([]byte, 2)
			binary.LittleEndian.PutUint16(crcBuf, sectorCRC)
			packet = append(packet, crcBuf...)
		}

		_, err := o.fwChar.WriteWithoutResponse(packet)
		if err != nil {
			return err
		}
		// Small delay to prevent saturating the device/BLE stack
		time.Sleep(30 * time.Millisecond)

		if isLastPacket {
			// Wait for sector ACK
		ACKLoop:
			for {
				select {
				case resp := <-o.fwChan:
					if len(resp) < 20 {
						fmt.Printf("\nReceived short notification on fwChan: %x\n", resp)
						continue
					}
					// Verify CRC
					expectedCRC := binary.LittleEndian.Uint16(resp[18:20])
					actualCRC := crc16(0, resp[0:18])
					if expectedCRC != actualCRC {
						return fmt.Errorf("CRC error in sector ACK: expected %04x, got %04x (raw: %x)", expectedCRC, actualCRC, resp)
					}

					var respIndex, ans uint16
					// Firmware ACKs in both standard and NimBLE branches of BLEOTA
					// appear to use [index_low, index_high, status_low, status_high, ...]
					// without the 0x03 prefix used in command ACKs.
					respIndex = binary.LittleEndian.Uint16(resp[0:2])
					ans = binary.LittleEndian.Uint16(resp[2:4])

					if respIndex == index && ans == 0 {
						// OK
						break ACKLoop
					} else if ans != 0 {
						return fmt.Errorf("sector ACK error: index=%d (expected %d), ans=%d (raw: %x)", respIndex, index, ans, resp)
					} else if respIndex < index {
						// Stale ACK, ignore it and wait for the correct one
						continue
					} else {
						return fmt.Errorf("unexpected sector index in ACK: index=%d (expected %d), raw: %x", respIndex, index, resp)
					}
				case <-time.After(60 * time.Second):
					return fmt.Errorf("timeout waiting for sector %d ACK (after 60s)", index)
				}
			}
		}

		offset += toRead
		sequence++
	}

	return nil
}

func (o *OTAClient) stopOTA() error {
	buf := make([]byte, 20)
	binary.LittleEndian.PutUint16(buf[0:2], StopOTA)
	// buf[2:18] is 0
	crc := crc16(0, buf[0:18])
	binary.LittleEndian.PutUint16(buf[18:20], crc)

	_, err := o.cmdChar.WriteWithoutResponse(buf)
	if err != nil {
		return err
	}

	return o.waitForCommandACK(StopOTA, "stop")
}

func findDevice(name string, address string) (bluetooth.Address, string, error) {
	type result struct {
		addr bluetooth.Address
		name string
	}
	foundChan := make(chan result, 1)

	fmt.Println("Scanning...")
	go func() {
		err := adapter.Scan(func(adapter *bluetooth.Adapter, scanRes bluetooth.ScanResult) {
			if name != "" {
				matched, err := path.Match(name, scanRes.LocalName())
				if err != nil || !matched {
					return
				}
			}
			if address != "" && scanRes.Address.String() != address {
				return
			}

			err := adapter.StopScan()
			if err != nil {
				// We don't return here because we want to send the address anyway
			}
			select {
			case foundChan <- result{addr: scanRes.Address, name: scanRes.LocalName()}:
			default:
			}
		})
		if err != nil {
			fmt.Printf("Scan error: %v\n", err)
		}
	}()

	select {
	case res := <-foundChan:
		return res.addr, res.name, nil
	case <-time.After(30 * time.Second):
		err := adapter.StopScan()
		if err != nil {
			return bluetooth.Address{}, "", err
		}
		return bluetooth.Address{}, "", errors.New("device discovery timeout")
	}
}
