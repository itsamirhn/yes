import asyncio
import os
import uuid
import logging
from urllib.parse import urlparse

import re
from base64 import b64encode, b64decode

import telegram

logging.basicConfig(
    level=logging.ERROR,
    format="%(asctime)s - %(levelname)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger(__name__)

BASE_URL: str = os.environ.get("BASE_URL", "https://api.telegram.org/bot")
BOT_TOKEN: str = os.environ.get("CLIENT_BOT_TOKEN", "")
CHAT_ID: str | int = os.environ.get("CHAT_ID", "")

if not BOT_TOKEN:
    raise ValueError("CLIENT_BOT_TOKEN environment variable is required")
if not CHAT_ID:
    raise ValueError("CHAT_ID environment variable is required")

bot = telegram.Bot(base_url=BASE_URL, token=BOT_TOKEN)
connects = []
# Track sequence numbers for each stream: {stream_id: {'send_seq': int, 'recv_seq': int, 'recv_buffer': dict}}
stream_seqs = {}

# Proxy settings
PROXY_HOST = "127.0.0.1"
PROXY_PORT = 8888


class AsyncBytesIO:
    def __init__(self):
        self._queue = asyncio.Queue()
        self._buffer = b""
        self._closed = False

    async def write(self, data: bytes):
        if self._closed:
            raise ValueError("I/O operation on closed file")
        await self._queue.put(data)

    async def read(self, size: int = -1):
        if self._closed:
            return b""

        while len(self._buffer) < size or size == -1:
            try:
                data = await asyncio.wait_for(self._queue.get(), timeout=3)
                self._buffer += data
            except asyncio.TimeoutError:
                break

        if size == -1:
            result = self._buffer
            self._buffer = b""
        else:
            result = self._buffer[:size]
            self._buffer = self._buffer[size:]

        return result

    def close(self):
        self._closed = True


async def open_connection(host: str, port: int):
    request_id = uuid.uuid4().hex
    await bot.send_message(chat_id=CHAT_ID, text=f"CONNECT {request_id} {host} {port}")

    while not any(request_id == connect[0] for connect in connects):
        await asyncio.sleep(0.01)

    rb, wb = next(
        (connect[2], connect[3]) for connect in connects if connect[0] == request_id
    )

    return rb, wb


async def handle_ok(message: telegram.Message):
    if not message.text:
        return

    group = re.search(r"^OK ([^\s]+) ([^\s]+)$", message.text)
    if group is None:
        return

    logger.info(f"Received OK message: {message.text}")

    request_id = group.group(1)
    stream_id = group.group(2)

    if any(request_id == connect[0] for connect in connects):
        logger.warning(f"Already connected with request_id {request_id}")
        return

    rb = AsyncBytesIO()

    class AsyncWriteBuffer:
        def __init__(self):
            self._buffer = b""
            self._buffer_size = 4096 - len(f"SEND {stream_id} 9999 ".encode("utf-8"))

        async def write(self, data):
            self._buffer += data
            # Auto-flush if buffer gets too large
            if len(self._buffer) >= self._buffer_size:
                await self.flush()

        async def flush(self):
            if self._buffer:
                # Get and increment sequence number
                seq_num = stream_seqs[stream_id]["send_seq"]
                stream_seqs[stream_id]["send_seq"] += 1

                await bot.send_message(
                    chat_id=CHAT_ID,
                    text=f"SEND {stream_id} {seq_num} {b64encode(self._buffer).decode('utf-8')}",
                )
                self._buffer = b""

        async def close(self):
            await self.flush()
            await bot.send_message(
                chat_id=CHAT_ID,
                text=f"CLOSE {stream_id}",
            )
            logger.info(f"Sent close command for stream_id {stream_id}")
            # Clean up sequence tracking
            if stream_id in stream_seqs:
                del stream_seqs[stream_id]

    wb = AsyncWriteBuffer()
    connects.append((request_id, stream_id, rb, wb))
    # Initialize sequence tracking for this stream
    stream_seqs[stream_id] = {"send_seq": 0, "recv_seq": 0, "recv_buffer": {}}


async def handle_recv(message: telegram.Message):
    if not message.text:
        return

    group = re.search(r"^RECV ([^\s]+) (\d+) (.+)$", message.text)
    if group is None:
        return

    stream_id = group.group(1)
    seq_num = int(group.group(2))
    data = b64decode(group.group(3))

    if not any(connect[1] == stream_id for connect in connects):
        logger.warning(f"No connection found for stream_id {stream_id}")
        return

    if stream_id not in stream_seqs:
        logger.warning(f"No sequence tracking found for stream_id {stream_id}")
        return

    rb = next(connect[2] for connect in connects if connect[1] == stream_id)

    # Check if this is the next expected sequence number
    expected_seq = stream_seqs[stream_id]["recv_seq"]
    if seq_num == expected_seq:
        # This is the expected sequence number, write immediately
        logger.debug(f"Received data for stream_id {stream_id} seq {seq_num}")
        await rb.write(data)
        stream_seqs[stream_id]["recv_seq"] += 1

        # Check if we have buffered messages that can now be written
        recv_buffer = stream_seqs[stream_id]["recv_buffer"]
        next_seq = stream_seqs[stream_id]["recv_seq"]
        while next_seq in recv_buffer:
            buffered_data = recv_buffer.pop(next_seq)
            logger.debug(
                f"Writing buffered data for stream_id {stream_id} seq {next_seq}"
            )
            await rb.write(buffered_data)
            stream_seqs[stream_id]["recv_seq"] += 1
            next_seq += 1
    else:
        # Out of order message, buffer it
        logger.debug(
            f"Buffering out-of-order message for stream_id {stream_id} seq {seq_num} (expected {expected_seq})"
        )
        stream_seqs[stream_id]["recv_buffer"][seq_num] = data


async def handle_close(message: telegram.Message):
    if not message.text:
        return

    group = re.search(r"^CLOSED (\w+)$", message.text)
    if group is None:
        return

    request_id = group.group(1)
    logger.info(f"Received close message: {message.text}")

    # Clean up sequence tracking for closed connections
    closed_streams = [connect[1] for connect in connects if connect[0] == request_id]
    for stream_id in closed_streams:
        if stream_id in stream_seqs:
            del stream_seqs[stream_id]

    connects[:] = [connect for connect in connects if connect[0] != request_id]


async def run_bot():
    last_id = None

    while True:
        try:
            await asyncio.sleep(0.001)

            updates = await bot.get_updates(
                offset=last_id + 1 if last_id else None,
                limit=10,
            )

            if not updates:
                continue

            last_id = max(update.update_id for update in updates)

            for update in updates:
                if update.message and update.message.text:
                    logger.debug(f"Received message: {update.message.text}")

                if update.message is None:
                    continue

                await handle_ok(update.message)
                await handle_recv(update.message)
                await handle_close(update.message)

        except Exception as e:
            logger.error(f"An error occurred: {e}", exc_info=True)
            await asyncio.sleep(5)


async def forward_data(reader, writer):
    """Forward data from reader to writer"""
    try:
        while True:
            data = await reader.read(4096)
            if not data:
                break

            # Check if this is our custom AsyncWriteBuffer or asyncio StreamWriter
            if hasattr(writer, "_buffer"):
                # Our custom AsyncWriteBuffer - write() is async
                await writer.write(data)
                await writer.flush()
            else:
                # Standard asyncio StreamWriter - write() is not async
                writer.write(data)
                await writer.drain()
    except Exception as e:
        logger.error(f"Error forwarding data: {e}")
    finally:
        try:
            if hasattr(writer, "_buffer"):
                await writer.close()
            else:
                # Standard asyncio StreamWriter
                writer.close()
        except:
            pass


async def handle_http_request(
    client_reader, client_writer, request_line: str, headers: dict
):
    """Handle HTTP request (non-CONNECT)"""
    try:
        # Parse the request line
        method, url, version = request_line.split(" ", 2)

        # Parse the URL to get host and port
        parsed_url = urlparse(
            url if url.startswith("http") else f"http://{headers.get('host', '')}{url}"
        )
        host = parsed_url.hostname
        port = parsed_url.port or (443 if parsed_url.scheme == "https" else 80)

        if not host:
            client_writer.write(b"HTTP/1.1 400 Bad Request\r\n\r\n")
            await client_writer.drain()
            return

        # Connect to target server through telegram bot
        server_reader, server_writer = await open_connection(host, port)

        # Forward the original request
        request_data = f"{method} {parsed_url.path or '/'}"
        if parsed_url.query:
            request_data += f"?{parsed_url.query}"
        request_data += f" {version}\r\n"

        # Add headers
        for header_name, header_value in headers.items():
            request_data += f"{header_name}: {header_value}\r\n"
        request_data += "\r\n"

        await server_writer.write(request_data.encode())
        await server_writer.flush()

        # Start bidirectional forwarding
        client_to_server = asyncio.create_task(
            forward_data(client_reader, server_writer)
        )
        server_to_client = asyncio.create_task(
            forward_data(server_reader, client_writer)
        )

        # Wait for either direction to complete
        await asyncio.gather(client_to_server, server_to_client, return_exceptions=True)

    except Exception as e:
        logger.error(f"Error handling HTTP request: {e}")
        try:
            client_writer.write(b"HTTP/1.1 500 Internal Server Error\r\n\r\n")
            await client_writer.drain()
        except:
            pass


async def handle_https_connect(client_reader, client_writer, host: str, port: int):
    """Handle HTTPS CONNECT request"""
    try:
        # Connect to target server through telegram bot
        server_reader, server_writer = await open_connection(host, port)

        # Send connection established response
        client_writer.write(b"HTTP/1.1 200 Connection established\r\n\r\n")
        await client_writer.drain()

        # Start bidirectional forwarding
        client_to_server = asyncio.create_task(
            forward_data(client_reader, server_writer)
        )
        server_to_client = asyncio.create_task(
            forward_data(server_reader, client_writer)
        )

        # Wait for either direction to complete
        await asyncio.gather(client_to_server, server_to_client, return_exceptions=True)

    except Exception as e:
        logger.error(f"Error handling HTTPS CONNECT: {e}")
        try:
            client_writer.write(b"HTTP/1.1 500 Internal Server Error\r\n\r\n")
            await client_writer.drain()
        except:
            pass


async def handle_client(client_reader, client_writer):
    """Handle incoming client connection"""
    try:
        # Read the first line (request line)
        request_line = await client_reader.readline()
        if not request_line:
            return

        request_line = request_line.decode().strip()

        # Read headers
        headers = {}
        while True:
            line = await client_reader.readline()
            if not line or line == b"\r\n":
                break

            line = line.decode().strip()
            if ":" in line:
                key, value = line.split(":", 1)
                headers[key.strip().lower()] = value.strip()

        # Parse request
        parts = request_line.split(" ")
        if len(parts) < 3:
            client_writer.write(b"HTTP/1.1 400 Bad Request\r\n\r\n")
            await client_writer.drain()
            return

        method = parts[0].upper()

        if method == "CONNECT":
            # HTTPS tunnel
            host_port = parts[1]
            if ":" in host_port:
                host, port_str = host_port.split(":", 1)
                port = int(port_str)
            else:
                host = host_port
                port = 443

            await handle_https_connect(client_reader, client_writer, host, port)
        else:
            # HTTP request
            await handle_http_request(
                client_reader, client_writer, request_line, headers
            )

    except Exception as e:
        logger.error(f"Error handling client: {e}")
    finally:
        try:
            client_writer.close()
        except:
            pass


async def run_proxy():
    """Run the HTTP/HTTPS proxy server"""
    server = await asyncio.start_server(handle_client, PROXY_HOST, PROXY_PORT)

    print(f"Proxy server running on {PROXY_HOST}:{PROXY_PORT}")
    print(f"Configure your browser to use HTTP proxy: {PROXY_HOST}:{PROXY_PORT}")

    async with server:
        await server.serve_forever()


async def test_connection():
    rb, wb = await open_connection("httpbin.org", 80)

    await wb.write(b"GET / HTTP/1.1\r\nHost: httpbin.org\r\n\r\n")
    await wb.flush()

    data = b""
    while True:
        chunk = await rb.read(4096)
        if not chunk:
            break
        data += chunk

    print("Received data:")
    print(data.decode("utf-8"))


async def main():
    print("Starting Telegram Bot Proxy...")

    # Start bot in background
    bot_task = asyncio.create_task(run_bot())
    await asyncio.sleep(2)

    # Test the connection first
    # print("Testing connection...")
    # await test_connection()

    # Start proxy server
    proxy_task = asyncio.create_task(run_proxy())

    # Keep both running
    await asyncio.gather(bot_task, proxy_task, return_exceptions=True)


if __name__ == "__main__":
    asyncio.run(main())
