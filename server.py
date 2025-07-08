import asyncio
import os
import uuid
import re
import logging
from base64 import b64encode, b64decode

import telegram

logging.basicConfig(
    level=logging.ERROR,
    format="%(asctime)s - %(levelname)s - %(message)s",
    datefmt="%Y-%m-%d %H:%M:%S",
)
logger = logging.getLogger(__name__)

BASE_URL: str = os.environ.get("BASE_URL", "https://api.telegram.org/bot")
BOT_TOKEN: str = os.environ.get("SERVER_BOT_TOKEN", "")

if not BOT_TOKEN:
    raise ValueError("SERVER_BOT_TOKEN environment variable is required")

connects = {}
# Track sequence numbers for each stream: {stream_id: {'send_seq': int, 'recv_seq': int, 'recv_buffer': dict}}
stream_seqs = {}


async def run():
    async with telegram.Bot(base_url=BASE_URL, token=BOT_TOKEN) as bot:
        last_id = None

        async def connect(message: telegram.Message):
            if not message.text:
                return

            group = re.search(r"^CONNECT ([^\s]+) ([^\s]+) (\d+)$", message.text)
            if group is None:
                return

            request_id = group.group(1)
            host = group.group(2)
            port = int(group.group(3))

            if request_id in connects:
                logger.warning(f"Already connected with request_id {request_id}")
                return

            logger.info(f"Connecting to {host}:{port} with request_id {request_id}")
            stream_id = uuid.uuid4().hex
            read, write = await asyncio.open_connection(host, port)
            connects[stream_id] = (read, write)
            # Initialize sequence tracking for this stream
            stream_seqs[stream_id] = {"send_seq": 0, "recv_seq": 0, "recv_buffer": {}}

            async def reader():
                while True:
                    data = await read.read(1024)
                    if not data:
                        logger.info(f"Connection closed for stream_id {stream_id}")
                        del connects[stream_id]
                        if stream_id in stream_seqs:
                            del stream_seqs[stream_id]
                        await bot.send_message(
                            chat_id=message.chat.id,
                            text=f"CLOSED {request_id} {stream_id}",
                        )
                        break
                    logger.debug(f"Received data on stream_id {stream_id}: {data}")

                    # Get and increment sequence number
                    seq_num = stream_seqs[stream_id]["send_seq"]
                    stream_seqs[stream_id]["send_seq"] += 1

                    await bot.send_message(
                        chat_id=message.chat.id,
                        text=f"RECV {stream_id} {seq_num} {b64encode(data).decode('utf-8')}",
                    )

            asyncio.create_task(reader())
            await bot.send_message(
                chat_id=message.chat.id, text=f"OK {request_id} {stream_id}"
            )

        async def send(message: telegram.Message):
            if not message.text:
                return

            group = re.search(r"^SEND (\w+) (\d+) (.+)$", message.text)
            if group is None:
                return

            stream_id = group.group(1)
            seq_num = int(group.group(2))
            data = b64decode(group.group(3))

            if stream_id not in connects:
                logger.warning(f"No connection found for stream_id {stream_id}")
                return

            if stream_id not in stream_seqs:
                logger.warning(f"No sequence tracking found for stream_id {stream_id}")
                return

            # Check if this is the next expected sequence number
            expected_seq = stream_seqs[stream_id]["recv_seq"]
            if seq_num == expected_seq:
                # This is the expected sequence number, send immediately
                logger.debug(
                    f"Sending data to stream_id {stream_id} seq {seq_num}: {data}"
                )
                _, write = connects[stream_id]
                write.write(data)
                await write.drain()
                stream_seqs[stream_id]["recv_seq"] += 1

                # Check if we have buffered messages that can now be sent
                recv_buffer = stream_seqs[stream_id]["recv_buffer"]
                next_seq = stream_seqs[stream_id]["recv_seq"]
                while next_seq in recv_buffer:
                    buffered_data = recv_buffer.pop(next_seq)
                    logger.debug(
                        f"Sending buffered data to stream_id {stream_id} seq {next_seq}: {buffered_data}"
                    )
                    _, write = connects[stream_id]
                    write.write(buffered_data)
                    await write.drain()
                    stream_seqs[stream_id]["recv_seq"] += 1
                    next_seq += 1
            else:
                # Out of order message, buffer it
                logger.debug(
                    f"Buffering out-of-order message for stream_id {stream_id} seq {seq_num} (expected {expected_seq})"
                )
                stream_seqs[stream_id]["recv_buffer"][seq_num] = data

        async def close(message: telegram.Message):
            if not message.text:
                return

            group = re.search(r"^CLOSE (\w+)$", message.text)
            if group is None:
                return

            stream_id = group.group(1)

            if stream_id not in connects:
                logger.warning(f"No connection found for stream_id {stream_id}")
                return

            logger.info(f"Closing connection for stream_id {stream_id}")
            _, write = connects[stream_id]
            write.close()
            del connects[stream_id]
            # Clean up sequence tracking
            if stream_id in stream_seqs:
                del stream_seqs[stream_id]

        while True:
            try:
                await asyncio.sleep(1)

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

                    await connect(update.message)
                    await send(update.message)
                    await close(update.message)
            except Exception as e:
                logger.error(f"An error occurred: {e}", exc_info=True)
                await asyncio.sleep(5)


if __name__ == "__main__":
    asyncio.run(run())
