# Telegram as Proxy PoC

A proof-of-concept implementation of creating a network proxy tunnel through Telegram (or similar messaging platforms like Bale messenger) to bypass network firewalls and censorship.

## 🎯 Concept

This project demonstrates how messaging platforms can be used as a communication channel to establish a proxy tunnel, enabling internet access in censored environments.

## 🔧 How It Works

### Architecture Overview

```
Laptop/Browser → Client Bot → Telegram Channel → Server Bot → Free Internet
   (Censored)      (Local)      (Messenger)      (Outside)    (Unrestricted)
```

### Components

1. **Client Bot** (`client.py`): Runs locally on your device behind the firewall
   - Acts as an HTTP/HTTPS proxy server (default: `127.0.0.1:8888`)
   - Forwards requests to the Telegram channel
   - Receives responses from the server bot through the channel

2. **Server Bot** (`server.py`): Runs outside the firewall (on a VPS or unrestricted server)
   - Monitors the Telegram channel for connection requests
   - Establishes actual connections to target websites
   - Relays data back through the Telegram channel

3. **Telegram Channel**: Acts as the communication bridge between both bots
   - Both bots must be added as administrators in the channel
   - Serves as a message queue for bidirectional data transfer

### Communication Protocol

The bots communicate using text-based commands through the Telegram channel:

- **CONNECT**: Client requests a new connection

  ```
  CONNECT {request_id} {host} {port}
  ```

- **OK**: Server acknowledges connection

  ```
  OK {request_id} {stream_id}
  ```

- **SEND**: Send data through the tunnel

  ```
  SEND {stream_id} {base64_encoded_data}
  ```

- **RECV**: Receive data from the tunnel

  ```
  RECV {stream_id} {base64_encoded_data}
  ```

- **CLOSE/CLOSED**: Close connection

  ```
  CLOSE {stream_id}
  CLOSED {stream_id}
  ```

### Why This Works

The key insight is that **messenger backends typically have unrestricted access to the internet**, even in censored regions. As long as the messaging platform itself is accessible, you can tunnel data through it.

#### Firewall Bypass Strategies

Depending on the firewall configuration, the server bot can use different methods:

- **Polling Mode** (getUpdates): Works when egress (outbound) filtering exists
  - Server actively polls Telegram for new messages
  - No inbound connections required

- **Webhook Mode**: Works when ingress (inbound) filtering exists
  - Telegram pushes updates to the server
  - More efficient but requires accepting inbound connections

This flexibility means that **as long as either ingress OR egress is not completely blocked**, the tunnel can function.

## 🚀 Setup

### Prerequisites

- Python 3.12 or higher
- Two Telegram bot tokens (via [@BotFather](https://t.me/botfather))
- A Telegram channel where both bots are administrators
- A server outside the firewall for running the server bot

### Installation

1. Clone the repository:

```bash
git clone <repository-url>
cd yes
```

2. Install dependencies using [uv](https://docs.astral.sh/uv/):

```bash
uv sync
```

### Configuration

#### For the Client Bot (local machine)

Set the following environment variables:

```bash
export CLIENT_BOT_TOKEN="your_client_bot_token"
export CHAT_ID="your_telegram_channel_id"  # numeric ID
export BASE_URL="https://api.telegram.org/bot"  # Optional: for custom Telegram API servers
```

#### For the Server Bot (external server)

Set the following environment variables:

```bash
export SERVER_BOT_TOKEN="your_server_bot_token"
export BASE_URL="https://api.telegram.org/bot"  # Optional: for custom Telegram API servers
```

### Running

1. **Start the server bot** (on your external server):

```bash
python server.py
```

2. **Start the client bot** (on your local machine):

```bash
python client.py
```

3. **Configure your browser** to use the proxy:
   - Proxy type: HTTP/HTTPS
   - Host: `127.0.0.1`
   - Port: `8888`

## 📝 Usage Example

Once both bots are running, you can:

1. Configure your browser to use `127.0.0.1:8888` as an HTTP proxy
2. Browse the internet normally
3. All traffic will be tunneled through the Telegram channel

### Testing

The client includes a test function to verify connectivity:

```python
# Uncomment in client.py main() function:
await test_connection()
```

## ⚠️ Limitations

- **Speed**: Limited by Telegram's rate limits and message processing speed
- **Latency**: Higher latency due to message queuing through Telegram
- **Reliability**: Dependent on Telegram's availability
- **Scale**: Not suitable for high-bandwidth applications

## 🔒 Security Considerations

- Data is base64-encoded but **not encrypted** beyond Telegram's own encryption
- Bot tokens should be kept secret
- This is a PoC and not intended for production use
- Consider adding additional encryption for sensitive data

## 🌐 Alternative Messengers

This works on any messenger with telegram backend such as Soroush or Bale messenger. For other messengers, the API has to be changed in the code.

Simply modify the `BASE_URL` to point to the alternative platform's API endpoint.

## 📚 Technical Details

### Data Flow Example

1. User makes an HTTPS request in their browser
2. Client bot receives the CONNECT request
3. Client sends `CONNECT {request_id} {host} {port}` to the channel
4. Server bot picks up the message
5. Server bot establishes a TCP connection to the target
6. Server responds with `OK {request_id} {stream_id}`
7. Data is exchanged using `SEND` and `RECV` commands with base64-encoded payloads
8. Connections are closed with `CLOSE`/`CLOSED` commands

### Connection Pooling

The implementation maintains connection pools on both sides to handle multiple simultaneous connections, enabling normal web browsing with multiple tabs and resources.

## 📄 License

This is a proof-of-concept project for educational purposes.

## 🤝 Contributing

Contributions are welcome! Possible improvements:

- Add end-to-end encryption for data payloads
- Implement connection pooling optimizations
- Add support for SOCKS proxy protocol
- Implement webhook support for the server bot
- Add bandwidth and latency metrics
- Support for other messaging platforms

## ⚖️ Disclaimer

This tool is provided for educational and research purposes. Users are responsible for complying with their local laws and the terms of service of messaging platforms. Use responsibly and ethically.
