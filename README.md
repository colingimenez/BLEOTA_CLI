# BLEOTA CLI Tool

This is a command-line tool to perform Over-The-Air (OTA) firmware updates on BLE devices supporting the [gb88/BLEOTA](https://github.com/gb88/BLEOTA) protocol.

## Features

- Scan for devices by name or address.
- Upload firmware to App partition or SPIFFS partition.
- Optional zlib compression to reduce transfer time.
- Progress bar and upload speed indication.
- CRC16 verification for data integrity.

## Installation

Ensure you have Go installed, then:

```bash
go mod tidy
go build -o bleota-cli .
```

## Usage

```bash
./bleota-cli --name "YourDeviceName" --file "firmware.bin"
# OR
./bleota-cli --address "00000000-0000-0000-0000-000000000000" --file "firmware.bin"

# With compression (recommended for faster uploads)
./bleota-cli --name "YourDeviceName" --file "firmware.bin" --compress
```

### Options

- `--name`: The BLE Local Name of the target device.
- `--address`: The BLE address of the target device (UUID on macOS, MAC address on Linux/Windows).
- `--file`: Path to the `.bin` firmware file.
- `--spiffs`: (Optional) Use this flag if you are uploading a SPIFFS image instead of an application firmware.
- `--compress`: (Optional) Compress the firmware with zlib before uploading to reduce transfer time.

## Requirements

- macOS, Linux, or Windows with Bluetooth support.
- On Linux, you may need `libbluetooth-dev` and root privileges (or correct setcap) for BLE access.
- On macOS, you may need to grant Terminal/IDE Bluetooth permissions.

## PlatformIO Integration

You can integrate this tool into your PlatformIO workflow for automated BLE OTA uploads:

```ini
[env:seeed_xiao_esp32s3]
platform = espressif32
board = seeed_xiao_esp32s3
framework = arduino
upload_protocol = custom
upload_command = ./bleota-cli -name <device name> -file $SOURCE -compress
```

Replace `<device name>` with your BLE device's name. This configuration allows you to upload firmware using PlatformIO's standard upload command, which will automatically use the BLE OTA method. The `-compress` flag is optional but recommended for faster transfers.
