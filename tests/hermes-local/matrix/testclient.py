# SPDX-License-Identifier: Apache-2.0
"""Minimal E2EE Matrix client standing in for Jase's phone in the local test.

Runs in a container built from the Hermes image (it has mautrix and libolm):

    python3 testclient.py <homeserver> <user> <password-file> <peer-user-id>

Logs in with a fresh device, opens an encrypted DM with <peer> (or joins the
room in $WORK/room if one exists and the peer is in it), prints every
decrypted message as one JSON line, and sends the text of any file dropped
into $WORK/outbox/ (deleted after sending). With PLAINTEXT=1 the room is
created without encryption, to check that Hermes refuses it.
"""

import asyncio
import json
import os
import sys
import time

from mautrix.client import Client
from mautrix.client.state_store import MemoryStateStore, MemorySyncStore
from mautrix.crypto import OlmMachine
from mautrix.crypto.store import MemoryCryptoStore
from mautrix.types import (EncryptedEvent, EventType, MessageEvent, RoomCreatePreset,
                           RoomEncryptionStateEventContent, EncryptionAlgorithm, StateEvent,
                           TextMessageEventContent, MessageType, TrustState, UserID)

WORK = os.environ.get("WORK", "/work")
PLAINTEXT = os.environ.get("PLAINTEXT") == "1"


class StateStore(MemoryStateStore):
    """The crypto machine also needs find_shared_rooms."""

    async def find_shared_rooms(self, user_id):
        return [room for room, members in self.members.items() if user_id in members]


def out(kind, **kw):
    print(json.dumps({"t": time.strftime("%H:%M:%S"), "kind": kind, **kw}), flush=True)


async def main():
    hs, user, pw_file, peer = sys.argv[1:5]
    peer = UserID(peer)
    state = StateStore()
    client = Client(base_url=hs, state_store=state, sync_store=MemorySyncStore())
    await client.login(identifier=user, password=open(pw_file).read().strip(),
                       initial_device_display_name=f"{user} test client")
    crypto = OlmMachine(client, MemoryCryptoStore(client.mxid, b"test"), state)
    # Test client: trust and share with unverified devices (Hermes' bot device
    # isn't cross-signed by us).
    crypto.share_keys_min_trust = TrustState.UNVERIFIED
    crypto.send_keys_min_trust = TrustState.UNVERIFIED
    client.crypto = crypto
    await crypto.load()
    await crypto.share_keys()
    out("ready", user=client.mxid, device=client.device_id)

    encrypted_ids = set()

    async def on_encrypted(evt: EncryptedEvent):
        # mautrix decrypts these itself (client.crypto is set) and re-dispatches
        # them as m.room.message; just note which events arrived encrypted.
        encrypted_ids.add(evt.event_id)

    async def on_message(evt: MessageEvent):
        if evt.sender == client.mxid:
            return
        await asyncio.sleep(0)  # let on_encrypted run first for the same event
        out("message", sender=evt.sender, room=evt.room_id, encrypted=evt.event_id in encrypted_ids,
            msgtype=str(evt.content.msgtype), body=evt.content.body)

    async def on_member(evt: StateEvent):
        if evt.state_key == peer:
            out("peer_membership", membership=str(evt.content.membership))

    client.add_event_handler(EventType.ROOM_MESSAGE, on_message)
    client.add_event_handler(EventType.ROOM_ENCRYPTED, on_encrypted)
    client.add_event_handler(EventType.ROOM_MEMBER, on_member)
    client.ignore_initial_sync = False
    client.start(None)

    room_file = os.path.join(WORK, "room")
    if os.path.exists(room_file):
        room = open(room_file).read().strip()
        await client.join_room(room)
    else:
        initial = [] if PLAINTEXT else [{
            "type": "m.room.encryption", "state_key": "",
            "content": {"algorithm": "m.megolm.v1.aes-sha2"}}]
        room = await client.create_room(preset=RoomCreatePreset.TRUSTED_PRIVATE, invitees=[peer],
                                        is_direct=True, initial_state=initial)
        if not PLAINTEXT:
            await state.set_encryption_info(room, RoomEncryptionStateEventContent(
                algorithm=EncryptionAlgorithm.MEGOLM_V1))
        open(room_file, "w").write(room)
    out("room", room=room, encrypted=not PLAINTEXT)

    outbox = os.path.join(WORK, "outbox")
    os.makedirs(outbox, exist_ok=True)
    while True:
        for name in sorted(os.listdir(outbox)):
            path = os.path.join(outbox, name)
            text = open(path).read().strip()
            os.remove(path)
            try:
                await client.send_message(room, TextMessageEventContent(msgtype=MessageType.TEXT, body=text))
                out("sent", body=text)
            except Exception as e:
                out("send_failed", error=f"{type(e).__name__}: {e}")
        await asyncio.sleep(1)


asyncio.run(main())
