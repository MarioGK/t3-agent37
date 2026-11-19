import asyncio
import json

from aiohttp import ClientSession, web
from aiohttp.test_utils import TestServer

from t3_agent37.relay import application


async def check():
    async def http(request):
        return web.json_response({
            "target": request.raw_path,
            "key": request.headers.get("X-Agent37-Key"),
            "auth": request.headers.get("Authorization"),
            "method": request.method,
            "payload": await request.json(),
        })

    async def websocket(request):
        assert request.headers["X-Agent37-Key"] == "fixture-key"
        connection = web.WebSocketResponse(protocols=("fixture",))
        await connection.prepare(request)
        async for message in connection:
            if isinstance(message.data, str):
                await connection.send_str(message.data)
            else:
                await connection.send_bytes(message.data)
        return connection

    service = web.Application()
    service.router.add_post("/check", http)
    service.router.add_get("/ws", websocket)
    port = 43737
    host = f"127.0.0.1:{port}"
    async with TestServer(service) as upstream:
        relay = application(str(upstream.make_url("/")).rstrip("/"), "fixture-key", port)
        async with TestServer(relay) as downstream, ClientSession() as client:
            args = {"Host": host, "Authorization": "Bearer native-t3", "X-Agent37-Key": "ignored"}
            response = await client.post(downstream.make_url("/check?q=value"), headers=args, json={"a": 1})
            assert response.status == 200
            assert await response.json() == {
                "target": "/check?q=value", "key": "fixture-key", "auth": "Bearer native-t3",
                "method": "POST", "payload": {"a": 1},
            }
            response = await client.get(downstream.make_url("/check"), headers={"Host": "outside.invalid"})
            assert response.status == 403
            response = await client.get(downstream.make_url("/check"), headers={
                "Host": host, "Origin": "https://outside.invalid",
            })
            assert response.status == 403
            async with client.ws_connect(downstream.make_url("/ws"), headers=args, protocols=("fixture",)) as ws:
                assert ws.protocol == "fixture"
                await ws.send_str(json.dumps({"t3": "test"}))
                assert await ws.receive_json() == {"t3": "test"}
                await ws.send_bytes(b"binary")
                assert await ws.receive_bytes() == b"binary"


def test_http_websocket_and_loopback_origin():
    asyncio.run(check())
