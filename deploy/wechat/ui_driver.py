#!/usr/bin/python3
"""Conservative AT-SPI controller for the official Linux WeChat client."""

import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

import pyatspi


MAX_REQUEST_BYTES = 1_048_576
MAX_TREE_NODES = 6_000
MAX_TREE_DEPTH = 18
MAX_SEMANTIC_TEXT = 16_384

AUTH_MARKERS = (
    "扫码登录", "扫描二维码", "二维码登录", "手机确认登录", "登录微信",
    "scan qr", "scan the qr", "log in to wechat", "confirm on phone",
)
CODE_MARKERS = ("验证码", "短信验证", "verification code", "sms code")
ERROR_MARKERS = ("崩溃", "无法启动", "版本过低", "crash", "unsupported version")
SEARCH_MARKERS = ("搜索", "search")
SEND_LABELS = ("发送", "send")
CONFIRM_LABELS = ("确定", "确认", "下一步", "continue", "confirm", "ok")
CLOSE_LABELS = ("返回", "关闭", "back", "close")
HIGH_RISK_MARKERS = (
    "支付", "付款", "转账", "红包", "授权", "允许", "身份验证", "实名认证",
    "购买", "下单", "订单", "充值", "提现", "借款", "签署", "签名",
    "账号安全", "修改密码", "删除账号", "注销账号", "绑定", "解绑", "举报",
    "pay", "payment", "transfer", "authorize", "permission", "identity verification",
    "purchase", "checkout", "order", "recharge", "withdraw", "loan", "sign agreement",
    "account security", "password", "delete account", "close account", "bind account", "report",
)
SHARE_MARKERS = ("分享", "转发", "发送给朋友", "share", "forward")
CONVERSATION_ROLES = ("list item", "table cell", "tree item")
MESSAGE_ROLES = ("text", "paragraph", "label", "list item", "link")
NON_CONVERSATION_LABELS = {
    "微信", "通讯录", "收藏", "朋友圈", "设置", "聊天", "小程序", "搜索",
    "chats", "contacts", "favorites", "settings", "search", "mini programs",
}


class ControlFailure(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code


def emit(value):
    sys.stdout.write(json.dumps(value, ensure_ascii=False, separators=(",", ":")))
    sys.stdout.flush()


def read_request():
    payload = sys.stdin.buffer.read(MAX_REQUEST_BYTES + 1)
    if len(payload) > MAX_REQUEST_BYTES:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "control request is too large")
    try:
        value = json.loads(payload or b"{}")
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ControlFailure("CLIENT_INCOMPATIBLE", f"invalid control request: {exc}")
    if not isinstance(value, dict):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "control request must be an object")
    return value


def safe_name(node):
    try:
        return str(node.name or "").strip()
    except Exception:
        return ""


def safe_description(node):
    try:
        return str(node.description or "").strip()
    except Exception:
        return ""


def safe_role(node):
    try:
        return str(node.getRoleName() or "").lower()
    except Exception:
        return "unknown"


def safe_state(node, state):
    try:
        return bool(node.getState().contains(state))
    except Exception:
        return False


def child_at(node, index):
    try:
        return node.getChildAtIndex(index)
    except Exception:
        return None


def walk(root, root_path=()):
    queue = [(root, tuple(root_path), 0)]
    visited = 0
    while queue and visited < MAX_TREE_NODES:
        node, path, depth = queue.pop(0)
        if node is None:
            continue
        visited += 1
        yield node, path
        if depth >= MAX_TREE_DEPTH:
            continue
        try:
            count = min(int(node.childCount), 1_000)
        except Exception:
            count = 0
        for index in range(count):
            child = child_at(node, index)
            if child is not None:
                queue.append((child, path + (index,), depth + 1))


def desktop():
    return pyatspi.Registry.getDesktop(0)


def process_running():
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        if entry.name == str(os.getpid()):
            continue
        try:
            arguments = [value.lower() for value in (entry / "cmdline").read_bytes().split(b"\0") if value]
        except (OSError, PermissionError):
            continue
        basenames = [value.rsplit(b"/", 1)[-1] for value in arguments]
        if any(value in (b"wechat", b"xwechat", b"wechatappex") or value.startswith(b".mount_wechat") for value in basenames):
            return True
    return False


def application_roots():
    root = desktop()
    result = []
    try:
        count = min(int(root.childCount), 128)
    except Exception:
        count = 0
    for index in range(count):
        child = child_at(root, index)
        name = safe_name(child).lower()
        if "wechat" in name or "微信" in name or "xwechat" in name or "wmpf" in name or "xweb" in name:
            result.append((child, (index,)))
    # Some Qt builds expose an empty application name. A small marker scan is
    # safer than assuming every desktop window belongs to WeChat.
    for index in range(count):
        child = child_at(root, index)
        names = " ".join(safe_name(node) for node, _ in walk(child)).lower()
        if ("微信" in names or "wechat" in names) and not any(path == (index,) for _, path in result):
            result.append((child, (index,)))
    return result


def all_nodes():
    result = []
    for root, path in application_roots():
        result.extend(walk(root, path))
    return result


def collect_strings(nodes, limit=1_000):
    values = []
    seen = set()
    for node, _ in nodes:
        for value in (safe_name(node), safe_description(node)):
            normalized = " ".join(value.split())
            if normalized and normalized not in seen:
                values.append(normalized)
                seen.add(normalized)
                if len(values) >= limit:
                    return values
    return values


def bounds(node):
    try:
        extents = node.queryComponent().getExtents(pyatspi.DESKTOP_COORDS)
        values = (int(extents.x), int(extents.y), int(extents.width), int(extents.height))
        if values[2] <= 0 or values[3] <= 0:
            return None
        return values
    except Exception:
        return None


def geometry_contains(container, candidate):
    if container is None or candidate is None:
        return False
    root_x, root_y, root_width, root_height = container
    x, y, width, height = candidate
    center_x = x + width / 2
    center_y = y + height / 2
    return (
        root_x <= center_x <= root_x + root_width
        and root_y <= center_y <= root_y + root_height
    )


def path_is_within(path, root_path):
    path = tuple(path)
    root_path = tuple(root_path)
    return len(path) >= len(root_path) and path[:len(root_path)] == root_path


def leaf_candidates(candidates):
    """Drop active parent frames when a more specific active window exists."""
    return [
        item for item in candidates
        if not any(
            len(other[1]) > len(item[1]) and path_is_within(other[1], item[1])
            for other in candidates
        )
    ]


def unique_window(candidates):
    candidates = leaf_candidates(candidates)
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple WeChat windows are currently active")
    return candidates[0] if candidates else None


def active_surface_root():
    roots = application_roots()
    if not roots:
        raise ControlFailure("SURFACE_MISSING", "WeChat surface is not visible")

    active_dialogs = []
    active_windows = []
    visible_dialogs = []
    visible_windows = []
    for root, path in roots:
        for candidate, candidate_path in walk(root, path):
            role = safe_role(candidate)
            if role not in ("frame", "window", "dialog") or bounds(candidate) is None:
                continue
            active = safe_state(candidate, pyatspi.STATE_ACTIVE)
            showing = safe_state(candidate, getattr(pyatspi, "STATE_SHOWING", pyatspi.STATE_ACTIVE))
            visible = safe_state(candidate, getattr(pyatspi, "STATE_VISIBLE", pyatspi.STATE_ACTIVE))
            if role == "dialog":
                if active:
                    active_dialogs.append((candidate, candidate_path))
                if showing or visible:
                    visible_dialogs.append((candidate, candidate_path))
            else:
                if active:
                    active_windows.append((candidate, candidate_path))
                if showing or visible:
                    visible_windows.append((candidate, candidate_path))

    # A modal dialog is the current semantic viewport even if its parent frame
    # incorrectly retains STATE_ACTIVE (seen with some Qt accessibility builds).
    for candidates in (active_dialogs, active_windows, visible_dialogs, visible_windows):
        selected = unique_window(candidates)
        if selected is not None:
            return selected
    if len(roots) == 1 and bounds(roots[0][0]) is not None:
        return roots[0]
    raise ControlFailure("TARGET_AMBIGUOUS", "current WeChat window cannot be uniquely identified")


def scope_nodes(scope=None):
    root, root_path = scope or active_surface_root()
    return list(walk(root, root_path))


def visible_node(node, scope_geometry):
    showing_state = getattr(pyatspi, "STATE_SHOWING", None)
    if showing_state is not None and not safe_state(node, showing_state):
        return False
    return geometry_contains(scope_geometry, bounds(node))


def main_geometry(scope=None):
    if scope is not None:
        geometry = bounds(scope[0])
        if geometry is None:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "current WeChat window has no usable geometry")
        return geometry
    candidates = []
    for root, path in application_roots():
        for node, node_path in walk(root, path):
            if safe_role(node) not in ("frame", "window", "application"):
                continue
            geometry = bounds(node)
            if geometry is not None:
                candidates.append((geometry[2] * geometry[3], geometry, node, node_path))
    if not candidates:
        return (0, 0, 1440, 960)
    return max(candidates, key=lambda item: item[0])[1]


def probe():
    roots = application_roots()
    now = time.time()
    if not roots:
        state = "STARTING" if process_running() else "OFFLINE"
        return {"ok": True, "state": state, "reason": "WeChat accessibility tree is not available"}
    nodes = all_nodes()
    text = "\n".join(collect_strings(nodes)).lower()
    if any(marker in text for marker in ERROR_MARKERS):
        return {"ok": True, "state": "DEGRADED", "reason": "client error dialog is visible"}
    if any(marker in text for marker in AUTH_MARKERS) or find_qr_bounds(nodes):
        can_submit = any(marker in text for marker in CODE_MARKERS)
        auth_kind = "sms" if can_submit else "qr"
        return {
            "ok": True,
            "state": "AUTH_REQUIRED",
            "auth_kind": auth_kind,
            "prompt": "Complete verification in the official WeChat client",
            "can_submit_code": can_submit,
            "qr_bounds": find_qr_bounds(nodes),
            "observed_unix": now,
        }
    return {"ok": True, "state": "ONLINE", "observed_unix": now}


def find_qr_bounds(nodes):
    candidates = []
    for node, _ in nodes:
        geometry = bounds(node)
        if geometry is None:
            continue
        x, y, width, height = geometry
        name = (safe_name(node) + " " + safe_description(node)).lower()
        role = safe_role(node)
        square = abs(width - height) <= max(width, height) * 0.2
        hinted = "二维码" in name or "qr" in name
        if square and width >= 120 and height >= 120 and (hinted or role in ("image", "icon")):
            candidates.append((hinted, width * height, x, y, width, height))
    if not candidates:
        return None
    _, _, x, y, width, height = max(candidates)
    return {"x": x, "y": y, "width": width, "height": height}


def action_interface(node):
    try:
        interface = node.queryAction()
        return interface if interface.nActions > 0 else None
    except Exception:
        return None


def activate(node, preferred=None):
    interface = action_interface(node)
    if interface is None:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "target has no accessible action")
    preferred = tuple(value.lower() for value in (preferred or ("click", "press", "activate", "jump")))
    matching = []
    for index in range(interface.nActions):
        try:
            name = str(interface.getName(index) or "").lower()
        except Exception:
            name = ""
        if name in preferred or any(value in name for value in preferred):
            matching.append(index)
    if len(matching) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "semantic target exposes multiple matching actions")
    if len(matching) == 1 and interface.doAction(matching[0]):
        return
    if not matching and interface.nActions == 1 and interface.doAction(0):
        return
    raise ControlFailure("CLIENT_INCOMPATIBLE", "accessible action was rejected")


def preferred_action_index(node):
    interface = action_interface(node)
    if interface is None:
        return None
    preferred = ("click", "press", "activate", "jump")
    matching = []
    for index in range(interface.nActions):
        try:
            name = str(interface.getName(index) or "").lower()
        except Exception:
            name = ""
        if name in preferred or any(value in name for value in preferred):
            matching.append(index)
    if len(matching) == 1:
        return matching[0]
    if len(matching) > 1:
        return None
    return 0 if interface.nActions == 1 else None


def exact_action_nodes(label, scope=None):
    normalized = " ".join(str(label).split()).casefold()
    result = []
    scope = scope or active_surface_root()
    scope_geometry = main_geometry(scope)
    for node, path in scope_nodes(scope):
        if " ".join(safe_name(node).split()).casefold() != normalized:
            continue
        if action_interface(node) is not None and visible_node(node, scope_geometry):
            result.append((node, path))
    return result


def select_exact(label, scope=None):
    candidates = exact_action_nodes(label, scope)
    if not candidates:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "semantic target is not visible")
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple visible semantic targets have the same title")
    activate(candidates[0][0])
    return candidates[0]


def path_has_role(path, roles):
    for length in range(1, len(path)):
        try:
            node = resolve_path(path[:length])
        except ControlFailure:
            return False
        if safe_role(node) in roles:
            return True
    return False


def path_has_editable(path):
    for length in range(1, len(path)):
        try:
            node = resolve_path(path[:length])
        except ControlFailure:
            return False
        if safe_state(node, pyatspi.STATE_EDITABLE):
            return True
        try:
            node.queryEditableText()
            return True
        except Exception:
            pass
    return False


def visible_conversations():
    scope = active_surface_root()
    root_x, root_y, root_width, root_height = main_geometry(scope)
    candidates = []
    for node, path in scope_nodes(scope):
        title = safe_name(node)
        role = safe_role(node)
        geometry = bounds(node)
        action_index = preferred_action_index(node)
        if not title or title != title.strip() or "\n" in title or len(title) > 256:
            continue
        if title.casefold() in NON_CONVERSATION_LABELS or role not in CONVERSATION_ROLES:
            continue
        if geometry is None or action_index is None:
            continue
        x, y, width, height = geometry
        if x < root_x or y < root_y + 40 or y > root_y + root_height * 0.92:
            continue
        if x + width / 2 > root_x + root_width * 0.46 or height < 18 or height > 180:
            continue
        if not path_has_role(path, ("list", "table", "tree", "list box")):
            continue
        locator = make_locator(path, "conversation", action_index, node=node)
        candidates.append({"title": title, "kind": "visible", "unread": 0, "locator": locator})
    counts = {}
    for item in candidates:
        counts[item["title"]] = counts.get(item["title"], 0) + 1
    result = []
    seen_locators = set()
    for item in candidates:
        if item["locator"] in seen_locators:
            continue
        seen_locators.add(item["locator"])
        item["ambiguous"] = counts[item["title"]] > 1
        result.append(item)
    return result


def select_conversation(title, locator):
    if not title or not locator:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "exact conversation title and locator are required")
    decoded = decode_locator(locator)
    if decoded.get("kind") != "conversation":
        raise ControlFailure("CLIENT_INCOMPATIBLE", "conversation locator has the wrong semantic kind")
    node = resolve_path(decoded["path"])
    if safe_name(node) != title or safe_role(node) not in CONVERSATION_ROLES:
        raise ControlFailure("ACTION_STALE", "conversation locator is stale")
    validate_node_signature(node, decoded)
    matching = [item for item in visible_conversations() if item["title"] == title]
    if len(matching) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "conversation title is not uniquely visible")
    if len(matching) != 1 or matching[0]["locator"] != locator:
        raise ControlFailure("ACTION_STALE", "conversation locator no longer identifies the visible target")
    activate_locator(node, decoded, "")
    time.sleep(0.35)
    scope = active_surface_root()
    verify_conversation_header(title, scope)
    return scope


def verify_conversation_header(title, scope=None):
    scope = scope or active_surface_root()
    root_x, root_y, root_width, root_height = main_geometry(scope)
    matches = []
    for node, path in scope_nodes(scope):
        if safe_name(node) != title:
            continue
        geometry = bounds(node)
        if geometry is None:
            continue
        x, y, width, height = geometry
        if (
            x + width / 2 > root_x + root_width * 0.46
            and y + height / 2 < root_y + root_height * 0.3
            and visible_node(node, (root_x, root_y, root_width, root_height))
        ):
            matches.append((node, path))
    if len(matches) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "selected conversation header cannot be uniquely verified")
    if not matches:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "selected conversation header is not visible")
    return matches[0]


def selected_conversation():
    scope = active_surface_root()
    conversations = visible_conversations()
    selected = []
    for item in conversations:
        node = resolve_path(decode_locator(item["locator"])["path"])
        if safe_state(node, pyatspi.STATE_SELECTED) or safe_state(node, pyatspi.STATE_ACTIVE):
            selected.append(item)
    if len(selected) == 1 and not selected[0].get("ambiguous"):
        verify_conversation_header(selected[0]["title"], scope)
        return selected[0]
    # Qt versions that omit STATE_SELECTED can still be resolved by an exact,
    # unique title shared by the left list and right-side header.
    root_x, root_y, root_width, root_height = main_geometry(scope)
    headers = []
    titles = {item["title"]: item for item in conversations if not item.get("ambiguous")}
    for node, _ in scope_nodes(scope):
        title = safe_name(node)
        if title not in titles:
            continue
        geometry = bounds(node)
        if geometry is None:
            continue
        x, y, width, height = geometry
        if x + width / 2 > root_x + root_width * 0.46 and y < root_y + root_height * 0.3:
            headers.append(titles[title])
    if len(headers) != 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "current conversation cannot be uniquely identified")
    return headers[0]


def metadata_text(value):
    lowered = value.strip().lower()
    return (
        bool(re.fullmatch(r"\d{1,2}:\d{2}", lowered))
        or bool(re.fullmatch(r"\d{4}[-/.]\d{1,2}[-/.]\d{1,2}", lowered))
        or lowered in {"以下为新消息", "new messages", "查看更多消息", "view more messages"}
    )


def visible_messages(request):
    title = str(request.get("conversation_title") or "")
    locator = str(request.get("conversation_locator") or "")
    if title or locator:
        scope = select_conversation(title, locator)
        conversation = {"title": title, "locator": locator}
    else:
        conversation = selected_conversation()
        title, locator = conversation["title"], conversation["locator"]
        scope = active_surface_root()
    root_x, root_y, root_width, root_height = main_geometry(scope)
    candidates = []
    seen = set()
    for node, path in scope_nodes(scope):
        text = safe_name(node)
        role = safe_role(node)
        geometry = bounds(node)
        if not text or not text.strip() or text == title or role not in MESSAGE_ROLES:
            continue
        if geometry is None or path_has_editable(path) or metadata_text(text):
            continue
        x, y, width, height = geometry
        center_x = x + width / 2
        if center_x < root_x + root_width * 0.38:
            continue
        if y < root_y + root_height * 0.12 or y > root_y + root_height * 0.9:
            continue
        if height <= 0 or height > root_height * 0.45:
            continue
        key = (text, x, y, width, height)
        if key in seen:
            continue
        seen.add(key)
        outgoing = x > root_x + root_width * 0.52
        lowered = text.lower()
        surface_kind = ""
        kind = "text"
        if "小程序" in lowered or "mini program" in lowered:
            kind, surface_kind = "miniprogram", "miniprogram"
        elif role == "link" or "http://" in lowered or "https://" in lowered:
            kind, surface_kind = "link", "web"
        elif role == "list item" and action_interface(node) is not None:
            kind, surface_kind = "card", "web"
        candidates.append((y, x, {
            "text": text,
            "kind": kind,
            "sender_name": "",
            "outgoing": outgoing,
            "accessible_label": text if surface_kind else "",
            "surface_kind": surface_kind,
            "confidence": 0.55 if outgoing else 0.45,
        }))
    candidates.sort(key=lambda item: (item[0], item[1]))
    return {
        "ok": True,
        "conversation_title": title,
        "conversation_locator": locator,
        "messages": [item[2] for item in candidates[:200]],
    }


def is_editable(node):
    try:
        node.queryEditableText()
        return True
    except Exception:
        return safe_state(node, pyatspi.STATE_EDITABLE)


def editable_nodes(scope=None, exclude_search=True):
    scope = scope or active_surface_root()
    scope_geometry = main_geometry(scope)
    result = []
    for node, path in scope_nodes(scope):
        name = (safe_name(node) + " " + safe_description(node)).lower()
        if exclude_search and any(marker in name for marker in SEARCH_MARKERS):
            continue
        geometry = bounds(node)
        if is_editable(node) and visible_node(node, scope_geometry):
            result.append((geometry[1], geometry[2] * geometry[3], node, path))
    result.sort(key=lambda item: (item[0], item[1]), reverse=True)
    return [(node, path) for _, _, node, path in result]


def unique_chat_editor(scope, title):
    verify_conversation_header(title, scope)
    root_x, root_y, root_width, root_height = main_geometry(scope)
    candidates = []
    for node, path in editable_nodes(scope, exclude_search=False):
        geometry = bounds(node)
        x, y, width, height = geometry
        center_x = x + width / 2
        center_y = y + height / 2
        if center_x <= root_x + root_width * 0.46 or center_y <= root_y + root_height * 0.45:
            continue
        if width < 80 or height < 18 or height > root_height * 0.5:
            continue
        candidates.append((node, path))
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple message editors are visible in the active chat pane")
    if not candidates:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "message editor is not uniquely accessible in the active chat pane")
    return candidates[0]


def set_text(node, text):
    if "\0" in text:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "text contains a NUL byte")
    try:
        node.queryComponent().grabFocus()
    except Exception:
        pass
    try:
        if node.queryEditableText().setTextContents(text):
            return
    except Exception:
        pass
    subprocess.run(
        ["xclip", "-selection", "clipboard", "-t", "UTF8_STRING", "-i"],
        input=text.encode("utf-8"), check=True,
    )
    send_key("v", control=True)


def text_contents(node):
    try:
        interface = node.queryText()
        count = getattr(interface, "characterCount", None)
        if count is None:
            count = interface.getCharacterCount()
        count = int(count)
        if count < 0 or count > MAX_REQUEST_BYTES:
            return False, ""
        return True, str(interface.getText(0, count))
    except Exception:
        return False, ""


def verify_text_contents(node, expected, required=True, timeout=0.75):
    deadline = time.monotonic() + timeout
    readable = False
    actual = ""
    while True:
        available, value = text_contents(node)
        if available:
            readable = True
            actual = value
            if actual == expected:
                return
        if time.monotonic() >= deadline:
            break
        time.sleep(0.05)
    if readable:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "editable text did not match the requested value")
    if required:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "editable text cannot be verified safely")


def send_key(key, control=False, alt=False):
    from Xlib import XK, display
    from Xlib.ext import xtest
    connection = display.Display()
    modifiers = []
    if control:
        modifiers.append(connection.keysym_to_keycode(XK.string_to_keysym("Control_L")))
    if alt:
        modifiers.append(connection.keysym_to_keycode(XK.string_to_keysym("Alt_L")))
    keycode = connection.keysym_to_keycode(XK.string_to_keysym(key))
    if keycode == 0:
        connection.close()
        raise ControlFailure("CLIENT_INCOMPATIBLE", "keyboard key is unavailable")
    for modifier in modifiers:
        xtest.fake_input(connection, 2, modifier)
    xtest.fake_input(connection, 2, keycode)
    xtest.fake_input(connection, 3, keycode)
    for modifier in reversed(modifiers):
        xtest.fake_input(connection, 3, modifier)
    connection.sync()
    connection.close()


def validate_attachment_paths(paths):
    if not isinstance(paths, list) or len(paths) > 20:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid attachment list")
    result = []
    allowed = Path("/wechatcopilot/runtime/outbox").resolve()
    for raw in paths:
        try:
            path = Path(str(raw)).resolve(strict=True)
        except (OSError, RuntimeError) as exc:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "attachment is not accessible") from exc
        try:
            path.relative_to(allowed)
        except ValueError as exc:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "attachment is outside the runtime outbox") from exc
        if not path.is_file():
            raise ControlFailure("CLIENT_INCOMPATIBLE", "attachment is not a regular file")
        result.append(path)
    return result


def stage_clipboard_files(paths):
    uris = [path.as_uri() for path in validate_attachment_paths(paths)]
    subprocess.run(
        ["xclip", "-selection", "clipboard", "-t", "text/uri-list", "-i"],
        input=("\r\n".join(uris) + "\r\n").encode("utf-8"), check=True,
    )
    send_key("v", control=True)


def named_nodes(labels, scope=None, require_action=True, predicate=None):
    normalized = {" ".join(label.split()).casefold() for label in labels}
    scope = scope or active_surface_root()
    scope_geometry = main_geometry(scope)
    candidates = []
    for node, path in scope_nodes(scope):
        label = " ".join(safe_name(node).split()).casefold()
        if label not in normalized or not visible_node(node, scope_geometry):
            continue
        if require_action and action_interface(node) is None:
            continue
        if predicate is not None and not predicate(node, path):
            continue
        candidates.append((node, path))
    return candidates


def unique_named_node(labels, scope=None, predicate=None, purpose="semantic button"):
    candidates = named_nodes(labels, scope, predicate=predicate)
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", f"multiple visible {purpose} targets are present")
    if not candidates:
        raise ControlFailure("CLIENT_INCOMPATIBLE", f"required {purpose} is not visible")
    return candidates[0]


def unique_chat_send(scope, editor_geometry):
    root_x, root_y, root_width, root_height = main_geometry(scope)
    editor_y = editor_geometry[1]

    def in_chat_footer(node, _path):
        x, y, width, height = bounds(node)
        return (
            x + width / 2 > root_x + root_width * 0.5
            and y + height / 2 > root_y + root_height * 0.5
            and y + height / 2 >= editor_y - 32
        )

    return unique_named_node(SEND_LABELS, scope, in_chat_footer, "chat send button")


def click_named(labels, scope=None, predicate=None, purpose="semantic button"):
    node, path = unique_named_node(labels, scope, predicate, purpose)
    activate(node)
    return node, path


def decode_locator(value):
    if not isinstance(value, str) or len(value) > 2_048:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid action locator")
    try:
        padding = "=" * (-len(value) % 4)
        decoded = base64.urlsafe_b64decode(value + padding)
        locator = json.loads(decoded)
    except (ValueError, TypeError, json.JSONDecodeError) as exc:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid action locator") from exc
    if not isinstance(locator, dict):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid action locator")
    path = locator.get("path")
    if not isinstance(path, list) or len(path) > MAX_TREE_DEPTH or any(not isinstance(i, int) or i < 0 or i > 1_000 for i in path):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid accessibility path")
    return locator


def resolve_path(path):
    node = desktop()
    for index in path:
        node = child_at(node, index)
        if node is None:
            raise ControlFailure("ACTION_STALE", "accessibility action is stale")
    return node


def action_name(interface, index):
    try:
        return str(interface.getName(index) or "")
    except Exception:
        return ""


def node_semantics(node, path, action_index=None):
    action = None
    if action_index is not None:
        interface = action_interface(node)
        if interface is None or action_index < 0 or action_index >= interface.nActions:
            raise ControlFailure("ACTION_STALE", "surface action is no longer available")
        action = {
            "index": action_index,
            "name": action_name(interface, action_index),
            "count": int(interface.nActions),
        }
    editable = is_editable(node)
    text_available, text_value = text_contents(node) if editable else (False, "")
    return {
        "path": list(path),
        "name": safe_name(node),
        "description": safe_description(node),
        "role": safe_role(node),
        "bounds": list(bounds(node) or ()),
        "action": action,
        "editable": editable,
        "text": text_value if text_available else None,
    }


def semantic_digest(value):
    payload = json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return hashlib.sha256(payload).hexdigest()


def node_signature(node, path, action_index=None):
    return semantic_digest(node_semantics(node, path, action_index))


def validate_node_signature(node, locator):
    if locator.get("version") != 2 or not isinstance(locator.get("node_signature"), str):
        raise ControlFailure("ACTION_STALE", "locator predates the current accessibility snapshot")
    action_index = locator.get("action")
    if action_index is not None and not isinstance(action_index, int):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid locator action")
    if node_signature(node, locator["path"], action_index) != locator["node_signature"]:
        raise ControlFailure("ACTION_STALE", "semantic target changed since it was observed")


def surface_generation(root, root_path, nodes):
    geometry = bounds(root)
    visible = [(node, path) for node, path in nodes if visible_node(node, geometry)]
    interactive = []
    for node, path in visible:
        interface = action_interface(node)
        if interface is not None:
            for index in range(min(interface.nActions, 8)):
                interactive.append(node_semantics(node, path, index))
        elif is_editable(node):
            interactive.append(node_semantics(node, path))
    return semantic_digest({
        "root": node_semantics(root, root_path),
        "strings": collect_strings(visible, limit=300),
        "interactive": interactive,
    })


def surface_kind_candidates(nodes, scope_geometry, kind):
    candidates = []
    for node, path in nodes:
        if not visible_node(node, scope_geometry):
            continue
        label = safe_name(node) or safe_description(node)
        role = safe_role(node)
        interface = action_interface(node)
        if interface is None:
            continue
        for index in range(min(interface.nActions, 8)):
            candidate_kind, _risk = classify_action(label, role, action_name(interface, index))
            if candidate_kind == kind:
                candidates.append((node, path, index))
    return candidates


def validate_surface_locator(locator, expected_kind=None, require_unique_kind=False):
    kind = locator.get("kind")
    if expected_kind is not None and kind != expected_kind:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface locator has the wrong semantic kind")
    generation = locator.get("surface_generation")
    if locator.get("version") != 2 or not isinstance(generation, str):
        raise ControlFailure("ACTION_STALE", "surface locator predates the current snapshot")
    scope = active_surface_root()
    root, root_path = scope
    if not path_is_within(locator["path"], root_path):
        raise ControlFailure("ACTION_STALE", "surface locator belongs to a different window")
    nodes = scope_nodes(scope)
    if surface_generation(root, root_path, nodes) != generation:
        raise ControlFailure("ACTION_STALE", "surface changed since the action was advertised")
    node = resolve_path(locator["path"])
    if not visible_node(node, main_geometry(scope)):
        raise ControlFailure("ACTION_STALE", "surface target is no longer visible")
    validate_node_signature(node, locator)
    action_index = locator.get("action")
    if action_index is None:
        if kind != "input" or not is_editable(node):
            raise ControlFailure("ACTION_STALE", "surface input no longer matches the locator")
    else:
        interface = action_interface(node)
        live_kind, _risk = classify_action(
            safe_name(node) or safe_description(node), safe_role(node),
            action_name(interface, action_index),
        )
        if live_kind != kind:
            raise ControlFailure("ACTION_STALE", "surface action semantics changed since the snapshot")
    if require_unique_kind:
        candidates = surface_kind_candidates(nodes, main_geometry(scope), kind)
        if len(candidates) > 1:
            raise ControlFailure("TARGET_AMBIGUOUS", f"multiple visible {kind} actions are present")
        if len(candidates) != 1:
            raise ControlFailure("ACTION_STALE", f"visible {kind} action is no longer available")
        _candidate_node, candidate_path, candidate_index = candidates[0]
        if (
            tuple(candidate_path) != tuple(locator["path"])
            or candidate_index != locator.get("action")
        ):
            raise ControlFailure("ACTION_STALE", f"visible {kind} action no longer matches the locator")
    return node, scope


def make_locator(path, kind, action_index=None, node=None, surface_generation_value=None):
    value = {"version": 2, "path": list(path), "kind": kind}
    if action_index is not None:
        value["action"] = action_index
    if node is not None:
        value["node_signature"] = node_signature(node, path, action_index)
    if surface_generation_value is not None:
        value["surface_generation"] = surface_generation_value
    payload = json.dumps(value, separators=(",", ":")).encode("utf-8")
    return base64.urlsafe_b64encode(payload).rstrip(b"=").decode("ascii")


def classify_action(label, role, action_name):
    text = (label + " " + action_name).lower()
    risk = "high" if any(marker in text for marker in HIGH_RISK_MARKERS) else "low"
    if any(marker in text for marker in SHARE_MARKERS):
        return "share", "medium"
    if role in ("text", "entry", "password text"):
        return "input", risk
    if any(marker in text for marker in CLOSE_LABELS):
        return "back", risk
    return "activate", risk


def surface_snapshot():
    root, root_path = active_surface_root()
    nodes = list(walk(root, root_path))
    scope_geometry = main_geometry((root, root_path))
    nodes = [(node, path) for node, path in nodes if visible_node(node, scope_geometry)]
    generation = surface_generation(root, root_path, nodes)
    strings = collect_strings(nodes, limit=300)
    semantic = "\n".join(strings)[:MAX_SEMANTIC_TEXT]
    title = safe_name(root)
    urls = re.findall(r"https?://[^\s<>\"]+", semantic)
    surface_text = (title + " " + semantic).lower()
    surface_high_risk = any(marker in surface_text for marker in HIGH_RISK_MARKERS)
    actions = []
    seen = set()
    for node, path in nodes:
        label = safe_name(node) or safe_description(node)
        role = safe_role(node)
        interface = action_interface(node)
        if interface is not None:
            for index in range(min(interface.nActions, 8)):
                try:
                    action_name = str(interface.getName(index) or "activate")
                except Exception:
                    action_name = "activate"
                kind, risk = classify_action(label, role, action_name)
                if surface_high_risk and kind not in ("back",):
                    risk = "high"
                locator = make_locator(
                    path, kind, index, node=node,
                    surface_generation_value=generation,
                )
                action_id = "a_" + hashlib.sha256((locator + label).encode("utf-8")).hexdigest()[:20]
                if action_id in seen:
                    continue
                seen.add(action_id)
                actions.append({
                    "id": action_id, "label": label or action_name,
                    "kind": kind, "risk": risk, "disabled": risk == "high",
                    "locator": locator,
                })
        else:
            try:
                node.queryEditableText()
            except Exception:
                continue
            locator = make_locator(
                path, "input", node=node,
                surface_generation_value=generation,
            )
            action_id = "a_" + hashlib.sha256((locator + label).encode("utf-8")).hexdigest()[:20]
            if action_id not in seen:
                seen.add(action_id)
                risk = "high" if surface_high_risk else "low"
                actions.append({
                    "id": action_id, "label": label or "Text input",
                    "kind": "input", "risk": risk, "disabled": risk == "high",
                    "locator": locator,
                })
        if len(actions) >= 80:
            break
    lower = (title + " " + semantic[:1_000]).lower()
    kind = "miniprogram" if "小程序" in lower or "wmpf" in lower else "web"
    return {
        "kind": kind, "title": title, "url": urls[0] if urls else "",
        "semantic_text": semantic, "actions": actions,
    }


def scope_identity(scope):
    root, root_path = scope
    return semantic_digest({
        "path": list(root_path),
        "name": safe_name(root),
        "role": safe_role(root),
        "bounds": list(bounds(root) or ()),
    })


def require_same_window(before, after):
    if scope_identity(before) != scope_identity(after):
        raise ControlFailure("ACTION_STALE", "active WeChat window changed during the operation")


def unique_auth_editor(scope):
    nodes = scope_nodes(scope)
    semantic = "\n".join(collect_strings(nodes)).lower()
    if not any(marker in semantic for marker in CODE_MARKERS):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "active window is not an authentication-code prompt")
    editors = editable_nodes(scope)
    marked = []
    for node, path in editors:
        text = (safe_name(node) + " " + safe_description(node)).lower()
        if any(marker in text for marker in CODE_MARKERS):
            marked.append((node, path))
    candidates = marked if marked else editors
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "authentication code field is ambiguous")
    if not candidates:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "authentication code field is not accessible")
    return candidates[0]


def submit_auth_code(code):
    if not re.fullmatch(r"[0-9]{4,10}", str(code)):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid authentication code")
    scope = active_surface_root()
    editor, editor_path = unique_auth_editor(scope)
    editor_role = safe_role(editor)
    editor_bounds = bounds(editor)
    # Resolve every semantic target before mutating the code field.
    unique_named_node(CONFIRM_LABELS, scope, purpose="authentication confirmation button")
    value = str(code)
    set_text(editor, value)
    verify_text_contents(editor, value, required=safe_role(editor) != "password text")
    current_scope = active_surface_root()
    require_same_window(scope, current_scope)
    current_editor = resolve_path(editor_path)
    if (
        not is_editable(current_editor)
        or not visible_node(current_editor, main_geometry(current_scope))
        or safe_role(current_editor) != editor_role
        or bounds(current_editor) != editor_bounds
    ):
        raise ControlFailure("ACTION_STALE", "authentication code field changed before confirmation")
    verify_text_contents(current_editor, value, required=safe_role(current_editor) != "password text")
    click_named(
        CONFIRM_LABELS, current_scope,
        purpose="authentication confirmation button",
    )


def send_message(request):
    title = str(request.get("conversation_title") or "").strip()
    conversation_locator = str(request.get("conversation_locator") or "")
    if not title:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "conversation title is required")
    share_locator = str(request.get("share_locator") or "")
    text = str(request.get("text") or "")
    attachments = request.get("attachments") or []
    if share_locator:
        if text or attachments:
            raise ControlFailure(
                "CLIENT_INCOMPATIBLE",
                "surface sharing cannot be combined with message text or attachments",
            )
        locator = decode_locator(share_locator)
        node, _surface_scope = validate_surface_locator(
            locator, expected_kind="share", require_unique_kind=True,
        )
        activate_locator(node, locator, "")
        time.sleep(0.5)
        # Share dialogs are separate surfaces and cannot reuse the main-list
        # locator. Exact matching remains mandatory and duplicate titles abort.
        share_scope = active_surface_root()
        unique_named_node(SEND_LABELS, share_scope, purpose="share send button")
        select_exact(title, share_scope)
        time.sleep(0.3)
        current_scope = active_surface_root()
        require_same_window(share_scope, current_scope)
        click_named(SEND_LABELS, current_scope, purpose="share send button")
        return

    validated_attachments = validate_attachment_paths(attachments)
    if not text and not validated_attachments:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "message text or an attachment is required")
    scope = select_conversation(title, conversation_locator)
    editor, editor_path = unique_chat_editor(scope, title)
    unique_chat_send(scope, bounds(editor))
    if text:
        set_text(editor, text)
        verify_text_contents(editor, text)
    else:
        # Never let a pre-existing draft hitch a ride on an attachment-only send.
        verify_text_contents(editor, "")
    if validated_attachments:
        try:
            editor.queryComponent().grabFocus()
        except Exception:
            pass
        stage_clipboard_files(validated_attachments)
        time.sleep(0.4)
    current_scope = active_surface_root()
    if scope_identity(scope) == scope_identity(current_scope):
        current_editor, current_editor_path = unique_chat_editor(current_scope, title)
        if tuple(current_editor_path) != tuple(editor_path):
            raise ControlFailure("ACTION_STALE", "message editor changed before send")
        if text:
            verify_text_contents(current_editor, text)
        send_node, _send_path = unique_chat_send(current_scope, bounds(current_editor))
    else:
        if not validated_attachments or tuple(scope[1][:1]) != tuple(current_scope[1][:1]):
            raise ControlFailure("ACTION_STALE", "active WeChat window changed before send")
        if safe_role(current_scope[0]) != "dialog":
            raise ControlFailure("ACTION_STALE", "attachment preview is not a uniquely active dialog")
        send_node, _send_path = unique_named_node(
            SEND_LABELS, current_scope, purpose="attachment send button",
        )
    activate(send_node)


def open_surface(request):
    title = str(request.get("conversation_title") or "").strip()
    conversation_locator = str(request.get("conversation_locator") or "")
    label = str(request.get("accessible_label") or "").strip()
    if not title or not conversation_locator or not label:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface needs an exact conversation and semantic label")
    scope = select_conversation(title, conversation_locator)
    select_exact(label, scope)
    time.sleep(1.0)
    return surface_snapshot()


def activate_locator(node, locator, text):
    kind = locator.get("kind")
    if kind == "input":
        set_text(node, str(text or ""))
        return
    interface = action_interface(node)
    index = locator.get("action")
    if interface is None or not isinstance(index, int) or index < 0 or index >= interface.nActions:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface action is no longer available")
    if not interface.doAction(index):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface action was rejected")


def act_surface(request):
    locator = decode_locator(request.get("locator"))
    node, _scope = validate_surface_locator(
        locator, require_unique_kind=locator.get("kind") == "share",
    )
    activate_locator(node, locator, request.get("text"))
    time.sleep(0.4)
    return surface_snapshot()


def close_surface():
    scope = active_surface_root()
    click_named(CLOSE_LABELS, scope, purpose="surface back or close button")


def dispatch(request):
    operation = request.get("operation")
    if operation == "probe":
        return probe()
    if operation == "submit_auth_code":
        submit_auth_code(request.get("text"))
        return {"ok": True}
    state = probe()
    if state.get("state") == "AUTH_REQUIRED":
        raise ControlFailure("AUTH_REQUIRED", "WeChat login is required")
    if state.get("state") not in ("ONLINE", "DEGRADED"):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "WeChat client is not ready")
    if operation == "send":
        send_message(request)
        return {"ok": True}
    if operation == "list_conversations":
        return {"ok": True, "conversations": visible_conversations()}
    if operation == "read_visible_messages":
        return visible_messages(request)
    if operation == "open_surface":
        return {"ok": True, "surface": open_surface(request)}
    if operation == "snapshot_surface":
        return {"ok": True, "surface": surface_snapshot()}
    if operation == "act_surface":
        return {"ok": True, "surface": act_surface(request)}
    if operation == "close_surface":
        close_surface()
        return {"ok": True}
    raise ControlFailure("CLIENT_INCOMPATIBLE", "unknown semantic operation")


def main():
    try:
        emit(dispatch(read_request()))
    except ControlFailure as exc:
        emit({"ok": False, "code": exc.code, "error": str(exc)})
    except subprocess.CalledProcessError:
        emit({"ok": False, "code": "CLIENT_INCOMPATIBLE", "error": "desktop helper failed"})
    except Exception as exc:
        emit({"ok": False, "code": "CLIENT_INCOMPATIBLE", "error": f"accessibility failure: {type(exc).__name__}"})


if __name__ == "__main__":
    main()
