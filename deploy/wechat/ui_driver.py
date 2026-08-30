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
import unicodedata

import pyatspi


MAX_REQUEST_BYTES = 1_048_576
MAX_TREE_NODES = 6_000
MAX_TREE_DEPTH = 18
MAX_SEMANTIC_TEXT = 16_384
MAX_SCREENSHOT_BYTES = 32 * 1024 * 1024
MAX_SURFACE_ELEMENTS = 400
MAX_SURFACE_ACTIONS = 120
MAX_SURFACE_ASSETS = 120
SURFACE_LOCATOR_TTL_SECONDS = 120
MAX_SCREENSHOT_PIXELS = 64_000_000
OCR_CONTEXT_MARGIN = 32
NAMED_SEARCH_TIMEOUT_SECONDS = 12
NAMED_OPEN_TIMEOUT_SECONDS = 20
NAMED_POLL_SECONDS = 0.35
MAIN_WINDOW_FOCUS_TIMEOUT_SECONDS = 3

AUTH_MARKERS = (
    "扫码登录", "扫描二维码", "二维码登录", "手机确认登录", "登录微信",
    "微信登录", "scan qr", "scan the qr", "log in to wechat", "wechat login",
    "confirm on phone",
)
QR_AUTH_MARKERS = (
    "扫码登录", "扫描二维码", "二维码登录", "scan qr", "scan the qr",
)
SAVED_ACCOUNT_LOGIN_LABELS = ("log in", "登录")
SAVED_ACCOUNT_SWITCH_LABELS = ("switch account", "切换账号")
SAVED_ACCOUNT_TRANSFER_LABELS = (
    "transfer files only", "仅传输文件", "仅传文件",
)
SAVED_ACCOUNT_ALTERNATIVE_LABEL_GROUPS = (
    SAVED_ACCOUNT_SWITCH_LABELS, SAVED_ACCOUNT_TRANSFER_LABELS,
)
SAVED_ACCOUNT_CONTROL_ROLES = ("push button", "button")
SAVED_ACCOUNT_ACTION_NAMES = ("click", "press", "activate")
SAVED_ACCOUNT_USER_PREFIXES = ("current user", "当前用户")
SAVED_ACCOUNT_AUTH_ACTION_ID = "continue_saved_account_login"
SAVED_ACCOUNT_AUTH_ACTION_TEMPLATE = {
    "label": "登录当前微信账号",
    "risk": "high",
    "confirmation": "请确认使用官方微信客户端显示的当前账号继续登录。",
    "requires_confirmation": True,
    "image_bound": True,
}
MAIN_SEARCH_LABELS = ("search", "搜索")
MAIN_NAVIGATION_LABEL_GROUPS = (
    ("chats", "聊天", "微信"),
    ("contacts", "通讯录"),
    ("favorites", "favourites", "收藏"),
    ("moments", "朋友圈"),
    ("mini programs", "miniprograms", "小程序"),
    ("settings", "设置"),
)
CODE_MARKERS = ("验证码", "短信验证", "短信验证码", "verification code", "sms code")
ERROR_MARKERS = ("崩溃", "无法启动", "版本过低", "crash", "unsupported version")
SEARCH_MARKERS = ("搜索", "search")
MINIPROGRAM_MARKERS = ("小程序", "mini program", "miniprogram", "wmpf")
NON_MINIPROGRAM_SECTION_MARKERS = (
    "联系人", "公众号", "聊天", "网页", "视频号", "文章", "contact",
    "official account", "chat", "web page", "channels", "article",
)
SEND_LABELS = ("发送", "send")
CONFIRM_LABELS = ("确定", "确认", "下一步", "continue", "confirm", "ok")
CLOSE_LABELS = ("返回", "关闭", "back", "close")
HIGH_RISK_MARKERS = (
    "支付", "付款", "转账", "红包", "授权", "允许", "身份验证", "实名认证",
    "购买", "下单", "订单", "充值", "提现", "借款", "签署", "签名",
    "账号安全", "修改密码", "删除账号", "注销账号", "关闭账号", "绑定", "解绑", "举报",
    "pay", "payment", "pay now", "buy now", "transfer", "authorize", "allow",
    "grant access", "grant permission", "give access", "permission", "identity verification",
    "purchase", "checkout", "order", "recharge", "withdraw", "loan", "sign agreement",
    "account security", "password", "delete account", "close account", "bind account", "report",
)
SECURITY_CHALLENGE_MARKERS = (
    "安全验证", "风险提示", "操作异常", "环境异常", "账号异常", "人脸识别",
    "人脸验证", "滑块验证", "拖动滑块", "请完成验证", "身份验证",
    "security check", "risk warning", "unusual activity", "verify your identity",
    "face verification", "captcha", "drag the slider",
)
SHARE_MARKERS = ("分享", "转发", "发送给朋友", "share", "forward")
DESTRUCTIVE_MARKERS = (
    "删除", "移除", "清空数据", "清空记录", "清空历史", "清除数据", "擦除",
    "抹掉数据", "销毁", "注销", "永久注销", "格式化", "重置账号", "重置账户",
    "撤销授权", "取消授权",
    "delete", "remove", "erase", "clear all", "clear history", "clear data", "wipe",
    "destroy", "reset account", "revoke access", "revoke authorization", "format",
)
EXTERNAL_WRITE_MARKERS = (
    "点赞", "评论", "收藏", "关注", "订阅", "发布", "提交", "上传", "回复", "保存", "发送",
    "like", "comment", "favorite", "favourite", "follow", "subscribe", "publish",
    "post", "submit", "upload", "reply", "save", "send",
)
NAVIGATE_MARKERS = (
    "查看", "详情", "打开", "进入", "更多", "下一页", "open", "view", "details",
    "more", "next", "jump", "navigate",
)
IMAGE_ROLES = ("image", "icon", "canvas", "drawing area")
CONVERSATION_ROLES = ("list item", "table cell", "tree item")
MESSAGE_ROLES = ("text", "paragraph", "label", "list item", "link")
NON_CONVERSATION_LABELS = {
    "微信", "通讯录", "收藏", "朋友圈", "设置", "聊天", "小程序", "搜索",
    "chats", "contacts", "favorites", "settings", "search", "mini programs",
}


class ControlFailure(Exception):
    def __init__(self, code, message, consumed=False):
        super().__init__(message)
        self.code = code
        self.consumed = bool(consumed)


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


def observable_accessible_identity(node, expected_process_id=None):
    """Return the re-observable AT-SPI object identity, never a Python address."""
    try:
        application = node.app
        bus_name = str(application.bus_name or "")
        object_path = str(node.path or "")
        process_id = int(node.get_process_id())
    except Exception:
        return ""
    if (
        not bus_name or len(bus_name) > 512
        or not object_path.startswith("/") or len(object_path) > 4_096
        or process_id <= 0
        or expected_process_id is not None and process_id != expected_process_id
        or any(ord(character) < 0x20 for character in bus_name + object_path)
    ):
        return ""
    return semantic_digest({
        "process_id": process_id, "bus_name": bus_name, "object_path": object_path,
    })


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
        if any(
            value in (b"wechat", b"xwechat", b"wechatappex", b"wechat.appimage")
            or value.startswith(b".mount_wechat")
            for value in basenames
        ):
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


def saved_account_auth_action(auth_generation):
    action = dict(SAVED_ACCOUNT_AUTH_ACTION_TEMPLATE)
    action["id"] = f"{SAVED_ACCOUNT_AUTH_ACTION_ID}.{auth_generation}"
    return action


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
    try:
        auth_scope = active_surface_root()
        auth_identity = verified_window_identity(auth_scope)
        auth_nodes = scope_nodes(auth_scope) if auth_identity.get("process_kind") == "wechat" else []
    except ControlFailure:
        return {
            "ok": True,
            "state": "DEGRADED",
            "reason": "active WeChat window ownership could not be verified",
            "observed_unix": now,
        }
    auth = authentication_evidence(auth_nodes) if auth_nodes else {
        "authenticated_surface": False, "sms": False, "qr": False,
    }
    if auth["authenticated_surface"]:
        can_submit = auth["sms"]
        if can_submit:
            auth_kind = "sms"
        elif auth.get("phone_confirmation"):
            auth_kind = "phone_confirmation"
        else:
            auth_kind = "qr"
        response = {
            "ok": True,
            "state": "AUTH_REQUIRED",
            "auth_kind": auth_kind,
            "prompt": "Complete verification in the official WeChat client",
            "can_submit_code": can_submit,
            "qr_bounds": find_qr_bounds(auth_nodes) if auth_kind == "qr" else None,
            "observed_unix": now,
        }
        if auth["saved_account_actionable"]:
            try:
                capture = capture_saved_account_confirmation()
            except ControlFailure:
                capture = None
            if capture is not None:
                generation = capture["auth_generation"]
                response.update({
                    "auth_generation": generation,
                    "screenshot_base64": base64.b64encode(capture["screenshot"]).decode("ascii"),
                    "screenshot_sha256": capture["screenshot_sha256"],
                    "actions": [saved_account_auth_action(generation)],
                })
        return response
    if main_wechat_ui_evidence(auth_nodes, main_geometry(auth_scope)):
        return {"ok": True, "state": "ONLINE", "observed_unix": now}
    return {
        "ok": True,
        "state": "DEGRADED",
        "reason": "official main WeChat UI structure is not yet available",
        "observed_unix": now,
    }


def find_qr_bounds(nodes):
    # A square image is common in posts and mini programs. Treat it as a QR
    # code only after the surrounding window has independently proved that it
    # is an authentication surface.
    if not authentication_evidence(nodes)["qr"]:
        return None
    window_geometries = [
        bounds(node) for node, _path in nodes
        if safe_role(node) in ("frame", "window", "dialog", "application")
        and bounds(node) is not None
    ]
    geometry = (
        max(window_geometries, key=lambda item: item[2] * item[3])
        if window_geometries else main_geometry()
    )
    candidates = []
    for node, _ in nodes:
        node_geometry = bounds(node)
        if (
            node_geometry is None
            or not visible_node(node, geometry)
            or not fully_contained_geometry(geometry, node_geometry)
        ):
            continue
        x, y, width, height = node_geometry
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


def authentication_evidence(nodes):
    window_geometries = [
        bounds(node) for node, _path in nodes
        if safe_role(node) in ("frame", "window", "dialog", "application") and bounds(node) is not None
    ]
    geometry = max(window_geometries, key=lambda item: item[2] * item[3]) if window_geometries else main_geometry()
    _root_x, root_y, _root_width, root_height = geometry
    auth_values = {normalized_exact(marker) for marker in AUTH_MARKERS}
    qr_auth_values = {normalized_exact(marker) for marker in QR_AUTH_MARKERS}
    code_values = {normalized_exact(marker) for marker in CODE_MARKERS}
    top_auth = []
    top_qr = []
    top_code = []
    code_editors = []
    confirms = []
    qr_images = []
    hinted_qr_images = []
    for node, _path in nodes:
        node_geometry = bounds(node)
        if (
            node_geometry is None
            or not visible_node(node, geometry)
            or not fully_contained_geometry(geometry, node_geometry)
        ):
            continue
        name = normalized_exact(safe_name(node) or safe_description(node))
        role = safe_role(node)
        near_top = node_geometry[1] <= root_y + root_height * 0.4
        compact_auth = name in auth_values
        compact_code = name in code_values
        if near_top and role in ("heading", "label", "text", "title bar"):
            if compact_auth:
                top_auth.append(node)
            if name in qr_auth_values:
                top_qr.append(node)
            if compact_code:
                top_code.append(node)
        if compact_code and is_editable(node):
            code_editors.append(node)
        if action_interface(node) is not None and has_semantic_marker(name, CONFIRM_LABELS):
            confirms.append(node)
        x, y, width, height = node_geometry
        square = abs(width - height) <= max(width, height) * 0.2
        hinted = "二维码" in name or has_semantic_marker(name, ("qr",))
        if square and width >= 120 and height >= 120 and (hinted or role in ("image", "icon")):
            qr_images.append(node)
            if hinted:
                hinted_qr_images.append(node)
    sms = bool(top_code) and len(code_editors) == 1 and bool(confirms)
    qr = (bool(top_qr) and bool(qr_images)) or (bool(top_auth) and bool(hinted_qr_images))
    phone = any(
        normalized_exact(safe_name(node) or safe_description(node))
        in {normalized_exact("手机确认登录"), normalized_exact("confirm on phone")}
        for node in top_auth
    )
    saved_account = saved_account_confirmation_analysis(nodes, geometry)
    return {
        "authenticated_surface": qr or sms or phone or saved_account["surface"],
        "sms": sms,
        "qr": qr,
        "phone_confirmation": phone or saved_account["surface"],
        "saved_account": saved_account["surface"],
        "saved_account_actionable": saved_account["target"] is not None,
    }


def saved_account_confirmation_evidence(nodes, geometry):
    return saved_account_confirmation_analysis(nodes, geometry)["surface"]


def saved_account_confirmation_target(nodes, geometry):
    return saved_account_confirmation_analysis(nodes, geometry)["target"]


def fully_contained_geometry(container, candidate):
    if container is None or candidate is None:
        return False
    root_x, root_y, root_width, root_height = container
    x, y, width, height = candidate
    return (
        x >= root_x and y >= root_y
        and x + width <= root_x + root_width
        and y + height <= root_y + root_height
    )


def saved_account_action_from_interface(interface):
    if interface is None or interface.nActions != 1:
        return None
    name = normalized_exact(action_name(interface, 0))
    if name not in SAVED_ACCOUNT_ACTION_NAMES:
        return None
    return {"index": 0, "name": name}


def saved_account_action_spec(node):
    return saved_account_action_from_interface(action_interface(node))


def saved_account_user_value(node):
    if safe_role(node) not in ("label", "text", "heading"):
        return ""
    for value in (safe_name(node), safe_description(node)):
        normalized = normalized_exact(value)
        for prefix in SAVED_ACCOUNT_USER_PREFIXES:
            if not normalized.startswith(prefix):
                continue
            suffix = normalized[len(prefix):].strip(" :-：")
            if suffix:
                return normalized
    return ""


def saved_account_control_record(node, path, geometry, matched):
    return {
        "node": node,
        "path": tuple(path),
        "geometry": geometry,
        "role": safe_role(node),
        "name": safe_name(node),
        "description": safe_description(node),
        "accessible_identity": observable_accessible_identity(node),
        "matched": set(matched),
        "action": saved_account_action_spec(node),
    }


def saved_account_control_layout(root_geometry, login, alternative):
    if not (
        fully_contained_geometry(root_geometry, login["geometry"])
        and fully_contained_geometry(root_geometry, alternative["geometry"])
    ):
        return False
    root_x, root_y, root_width, root_height = root_geometry
    login_x, login_y, login_width, login_height = login["geometry"]
    alternative_x, alternative_y, alternative_width, alternative_height = alternative["geometry"]
    login_center_x = login_x + login_width / 2
    login_center_y = login_y + login_height / 2
    alternative_center_x = alternative_x + alternative_width / 2
    alternative_center_y = alternative_y + alternative_height / 2
    return (
        root_x + root_width * 0.2 <= login_center_x <= root_x + root_width * 0.8
        and root_y + root_height * 0.2 <= login_center_y <= root_y + root_height * 0.85
        and root_x + root_width * 0.15 <= alternative_center_x <= root_x + root_width * 0.85
        and abs(alternative_center_x - login_center_x) <= root_width * 0.32
        and login_center_y < alternative_center_y <= login_center_y + root_height * 0.4
        and not rectangles_overlap(login["geometry"], alternative["geometry"], threshold=0.01)
    )


def saved_account_user_labels(nodes, root_geometry, login):
    login_x, login_y, login_width, _login_height = login["geometry"]
    login_center_x = login_x + login_width / 2
    root_width = root_geometry[2]
    result = []
    for node, path in nodes:
        value = saved_account_user_value(node)
        geometry = bounds(node)
        if not value or not visible_node(node, root_geometry):
            continue
        if not fully_contained_geometry(root_geometry, geometry):
            continue
        x, y, width, height = geometry
        if y + height > login_y or abs((x + width / 2) - login_center_x) > root_width * 0.25:
            continue
        result.append({
            "node": node,
            "path": tuple(path),
            "geometry": geometry,
            "role": safe_role(node),
            "name": safe_name(node),
            "description": safe_description(node),
            "accessible_identity": observable_accessible_identity(node),
            "value": value,
        })
    return result


def saved_account_common_container(nodes, records, root_geometry):
    paths = [record["path"] for record in records]
    container_path = paths[0][:-1]
    for path in paths[1:]:
        container_path = common_path(container_path, path[:-1])
    by_path = {tuple(path): node for node, path in nodes}
    container = by_path.get(container_path)
    if container is None or safe_role(container) not in (
        "application", "frame", "window", "dialog", "panel", "section", "grouping", "filler",
    ):
        return None
    container_geometry = bounds(container)
    if container_geometry is None or not fully_contained_geometry(root_geometry, container_geometry):
        return None
    if not all(fully_contained_geometry(container_geometry, record["geometry"]) for record in records):
        return None
    return container_path


def saved_account_record_semantics(record):
    result = {
        "path": list(record["path"]),
        "name": record["name"],
        "description": record["description"],
        "role": record["role"],
        "bounds": list(record["geometry"]),
        "accessible_identity": record.get("accessible_identity", ""),
    }
    if record.get("action") is not None:
        result["action"] = dict(record["action"])
    if record.get("value"):
        result["value"] = record["value"]
    return result


def saved_account_confirmation_analysis(nodes, geometry):
    if not compact_saved_account_window(geometry):
        return {"surface": False, "target": None}
    login_labels = {normalized_exact(label) for label in SAVED_ACCOUNT_LOGIN_LABELS}
    alternative_label_groups = tuple(
        {normalized_exact(label) for label in group}
        for group in SAVED_ACCOUNT_ALTERNATIVE_LABEL_GROUPS
    )
    alternative_labels = set().union(*alternative_label_groups)
    expected = login_labels | alternative_labels
    login_controls = []
    alternative_controls = [[] for _group in alternative_label_groups]
    for node, path in nodes:
        node_geometry = bounds(node)
        if (
            node_geometry is None
            or not visible_node(node, geometry)
            or not fully_contained_geometry(geometry, node_geometry)
        ):
            continue
        labels = {
            normalized_exact(safe_name(node)),
            normalized_exact(safe_description(node)),
        }
        labels.discard("")
        matched = labels & expected
        if not matched or safe_role(node) not in SAVED_ACCOUNT_CONTROL_ROLES:
            continue
        if action_interface(node) is None:
            continue
        record = saved_account_control_record(node, path, node_geometry, matched)
        if matched & login_labels:
            login_controls.append(record)
        for index, label_group in enumerate(alternative_label_groups):
            if matched & label_group:
                alternative_controls[index].append(record)

    alternatives = [item for group in alternative_controls for item in group]
    surface = any(
        saved_account_control_layout(geometry, login, alternative)
        and bool(saved_account_user_labels(nodes, geometry, login))
        for login in login_controls for alternative in alternatives
    )
    if not surface:
        return {"surface": False, "target": None}
    if (
        len(login_controls) != 1
        or any(len(group_controls) > 1 for group_controls in alternative_controls)
    ):
        return {"surface": True, "target": None}
    alternatives = [group[0] for group in alternative_controls if group]
    if not alternatives:
        return {"surface": True, "target": None}
    login = login_controls[0]
    user_labels = saved_account_user_labels(nodes, geometry, login)
    records = [login, *alternatives, *user_labels]
    if (
        len(user_labels) != 1
        or any(not record.get("accessible_identity") for record in records)
        or any(record.get("action") is None for record in (login, *alternatives))
        or not all(saved_account_control_layout(geometry, login, item) for item in alternatives)
        or saved_account_common_container(nodes, records, geometry) is None
    ):
        return {"surface": True, "target": None}
    control_paths = [record["path"] for record in records]
    if len(set(control_paths)) != len(control_paths):
        return {"surface": True, "target": None}
    if any(
        rectangles_overlap(left["geometry"], right["geometry"], threshold=0.01)
        for index, left in enumerate(records)
        for right in records[index + 1:]
    ):
        return {"surface": True, "target": None}
    signature = semantic_digest({
        "window_geometry": list(geometry),
        "login": saved_account_record_semantics(login),
        "alternatives": [saved_account_record_semantics(item) for item in alternatives],
        "user": saved_account_record_semantics(user_labels[0]),
    })
    login["static_signature"] = node_signature(login["node"], login["path"])
    target = {
        "login": login,
        "alternatives": alternatives,
        "user": user_labels[0],
        "signature": signature,
    }
    return {"surface": True, "target": target}


def saved_account_page_semantic_signature(nodes, geometry):
    visible = []
    image_signatures = []
    for node, path in nodes:
        node_geometry = bounds(node)
        if (
            node_geometry is None
            or not visible_node(node, geometry)
            or not fully_contained_geometry(geometry, node_geometry)
        ):
            continue
        record = {
            "path": list(path),
            "name": safe_name(node),
            "description": safe_description(node),
            "role": safe_role(node),
            "bounds": list(node_geometry),
            "accessible_identity": observable_accessible_identity(node),
        }
        visible.append(record)
        if safe_role(node) in IMAGE_ROLES:
            image_signatures.append(node_signature(node, path))
    return semantic_digest({
        "window_geometry": list(geometry),
        "visible_nodes": visible,
        "image_node_signatures": image_signatures,
    })


def compact_saved_account_window(geometry):
    if geometry is None:
        return False
    _x, _y, width, height = geometry
    return (
        240 <= width <= 720
        and 300 <= height <= 900
        and width * height <= 540_000
        and width <= height * 1.4
    )


def main_wechat_ui_evidence(nodes, geometry):
    if geometry is None:
        return False
    root_x, root_y, root_width, root_height = geometry
    if root_width < 640 or root_height < 480:
        return False
    search_labels = {normalized_exact(label) for label in MAIN_SEARCH_LABELS}
    navigation_groups = tuple(
        {normalized_exact(label) for label in group}
        for group in MAIN_NAVIGATION_LABEL_GROUPS
    )
    searches = []
    navigation = []
    nodes_by_path = {tuple(path): node for node, path in nodes}
    for node, path in nodes:
        node_geometry = bounds(node)
        if (
            node_geometry is None
            or not visible_node(node, geometry)
            or not fully_contained_geometry(geometry, node_geometry)
        ):
            continue
        labels = {
            normalized_exact(safe_name(node)),
            normalized_exact(safe_description(node)),
        }
        labels.discard("")
        x, y, width, height = node_geometry
        if (
            labels & search_labels
            and is_editable(node)
            and safe_role(node) in ("entry", "search box", "text")
            and y + height / 2 <= root_y + root_height * 0.35
            and x + width / 2 <= root_x + root_width * 0.65
        ):
            searches.append((node, tuple(path)))
        if action_interface(node) is None:
            continue
        matching_groups = {
            index for index, group in enumerate(navigation_groups) if labels & group
        }
        if len(matching_groups) != 1 or safe_role(node) not in (
            "push button", "button", "toggle button", "list item", "page tab",
        ):
            continue
        navigation.append((tuple(path), next(iter(matching_groups)), node_geometry))
    if len(searches) != 1:
        return False
    for index, (left_path, left_group, left_geometry) in enumerate(navigation):
        for right_path, right_group, right_geometry in navigation[index + 1:]:
            if left_group == right_group:
                continue
            container_path = common_path(left_path[:-1], right_path[:-1])
            container = nodes_by_path.get(container_path)
            container_geometry = bounds(container) if container is not None else None
            if (
                safe_role(container) not in ("panel", "section", "grouping", "filler", "tool bar")
                or container_geometry is None
                or not fully_contained_geometry(geometry, container_geometry)
                or not fully_contained_geometry(container_geometry, left_geometry)
                or not fully_contained_geometry(container_geometry, right_geometry)
            ):
                continue
            container_x, _container_y, container_width, _container_height = container_geometry
            if (
                container_width <= root_width * 0.45
                and container_x + container_width / 2 <= root_x + root_width * 0.4
            ):
                return True
    return False


def authentication_text(nodes):
    return authentication_evidence(nodes)["authenticated_surface"]


def authentication_context(nodes):
    return authentication_evidence(nodes)["authenticated_surface"]


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
    names = []
    for index in range(interface.nActions):
        try:
            name = " ".join(str(interface.getName(index) or "").lower().split())
        except Exception:
            name = ""
        names.append(name)
        if name in preferred:
            matching.append(index)
    if len(matching) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "semantic target exposes multiple matching actions")
    if len(matching) == 1 and interface.doAction(matching[0]):
        return
    if not matching and names == [""] and interface.doAction(0):
        return
    raise ControlFailure("CLIENT_INCOMPATIBLE", "accessible action was rejected")


def preferred_action_index(node):
    interface = action_interface(node)
    return preferred_action_index_from_interface(interface)


def preferred_action_index_from_interface(interface):
    if interface is None:
        return None
    preferred = ("click", "press", "activate", "jump")
    matching = []
    names = []
    for index in range(interface.nActions):
        try:
            name = " ".join(str(interface.getName(index) or "").lower().split())
        except Exception:
            name = ""
        names.append(name)
        if name in preferred:
            matching.append(index)
    if len(matching) == 1:
        return matching[0]
    if len(matching) > 1:
        return None
    return 0 if names == [""] else None


def named_launch_action_index(node):
    """Select only an unambiguously navigation-like result action."""
    interface = action_interface(node)
    return named_launch_action_index_from_interface(node, interface)


def named_launch_action_index_from_interface(node, interface):
    if interface is None:
        return None
    preferred = (
        "click", "press", "activate", "jump", "open", "open link", "enter", "view",
        "点击", "按下", "激活", "跳转", "打开", "进入", "查看",
    )
    matching = []
    empty = []
    for index in range(min(interface.nActions, 8)):
        name = " ".join(action_name(interface, index).strip().lower().split())
        _kind, risk, effect = classify_action_effect(
            safe_name(node), safe_role(node), name,
            description=safe_description(node), editable=is_editable(node),
        )
        if risk == "high" or effect in ("external_write", "high_risk"):
            continue
        if not name:
            empty.append(index)
        elif name in preferred:
            matching.append(index)
    if len(matching) == 1:
        return matching[0]
    # Some Qt builds expose one unnamed default action. Retain that narrow
    # compatibility case, but never reinterpret an explicitly named unknown
    # or mutating action as navigation.
    if not matching and interface.nActions == 1 and empty == [0]:
        return 0
    return None


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


def unique_exact_action_candidate(label, scope):
    candidates = exact_action_nodes(label, scope)
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple visible semantic targets have the same title")
    if not candidates:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "semantic target is not visible")
    node, path = candidates[0]
    action_index = preferred_action_index(node)
    if action_index is None:
        raise ControlFailure("TARGET_AMBIGUOUS", "semantic target action is ambiguous")
    return {
        "path": tuple(path), "action": action_index,
        "signature": node_signature(node, path, action_index),
    }


def activate_exact_action_candidate(candidate):
    node = resolve_path(candidate["path"])
    interface = action_interface(node)
    if interface is None:
        raise ControlFailure("ACTION_STALE", "semantic target action is unavailable")
    if node_signature(
        node, candidate["path"], candidate["action"], interface=interface,
    ) != candidate["signature"]:
        raise ControlFailure("ACTION_STALE", "semantic target changed before activation")
    expected_identity = candidate.get("accessible_identity")
    if expected_identity and observable_accessible_identity(node) != expected_identity:
        raise ControlFailure("ACTION_STALE", "accessible target changed before activation")
    if preferred_action_index_from_interface(interface) != candidate["action"]:
        raise ControlFailure("ACTION_STALE", "semantic target action changed before activation")
    action_label = action_name(interface, candidate["action"])
    _kind, risk, effect = classify_action_effect(
        safe_name(node), safe_role(node), action_label,
        description=safe_description(node), editable=is_editable(node),
    )
    if risk == "high" or effect in ("external_write", "high_risk"):
        raise ControlFailure("USER_ACTION_REQUIRED", "unsafe semantic target requires direct user interaction")
    if not interface.doAction(candidate["action"]):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "semantic target rejected activation")


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
    if verified_window_identity(scope).get("process_kind") != "wechat":
        raise ControlFailure("CLIENT_INCOMPATIBLE", "conversation access requires the official main WeChat window")
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


def message_surface_kind(node):
    if message_surface_action_index(node) is None:
        return ""
    text = safe_name(node)
    description = safe_description(node)
    lowered = normalize_cjk_spacing(" ".join((text, description))).lower()
    role = safe_role(node)
    if "小程序" in lowered or "mini program" in lowered:
        return "miniprogram"
    if role == "link" or "http://" in lowered or "https://" in lowered:
        return "web"
    if role == "list item":
        return "web"
    return ""


def message_surface_action_index(node):
    interface = action_interface(node)
    if interface is None:
        return None
    safe_names = {
        "click", "press", "activate", "jump", "open", "open link", "follow link",
        "点击", "按下", "激活", "跳转", "打开", "进入", "查看",
    }
    unsafe_markers = (
        "delete", "remove", "play", "pause", "download", "save", "share", "send",
        "submit", "call", "删除", "移除", "播放", "暂停", "下载", "保存", "分享",
        "发送", "提交", "通话",
    )
    matches = []
    for index in range(min(interface.nActions, 8)):
        name = " ".join(action_name(interface, index).strip().lower().split())
        if not name or any(marker in name for marker in unsafe_markers):
            continue
        if name not in safe_names:
            continue
        _kind, risk, effect = classify_action_effect(
            safe_name(node), safe_role(node), name,
            description=safe_description(node), editable=is_editable(node),
        )
        if risk == "high" or effect == "external_write":
            continue
        matches.append(index)
    return matches[0] if len(matches) == 1 else None


def validate_message_surface_candidate(label, expected_kind, encoded_locator, frame):
    locator = decode_locator(encoded_locator)
    if locator.get("kind") != "message_surface":
        raise ControlFailure("CLIENT_INCOMPATIBLE", "message surface locator has the wrong kind")
    if locator.get("version") != 4:
        raise ControlFailure("ACTION_STALE", "message surface locator predates observable identity binding")
    issued = locator.get("issued_unix")
    expires = locator.get("expires_unix")
    if (
        not isinstance(issued, int) or not isinstance(expires, int)
        or expires <= issued or expires - issued > SURFACE_LOCATOR_TTL_SECONDS
        or time.time() > expires
    ):
        raise ControlFailure("ACTION_STALE", "message surface locator has expired")
    scope = frame["scope"]
    if (
        locator.get("strict_frame") is not True
        or locator.get("source") != "atspi"
        or locator.get("window_identity") != frame.get("window_identity")
        or locator.get("scope_identity") != scope_identity(scope)
        or locator.get("scope_geometry") != list(main_geometry(scope))
        or locator.get("surface_generation") != frame.get("generation")
        or locator.get("rendered_frame_sha256") != frame.get("rendered_frame_sha256")
    ):
        raise ControlFailure("ACTION_STALE", "message surface snapshot changed since it was indexed")
    if not path_is_within(locator["path"], scope[1]):
        raise ControlFailure("ACTION_STALE", "message surface locator belongs to another conversation window")
    node = resolve_path(locator["path"])
    if safe_name(node) != label or message_surface_kind(node) != expected_kind:
        raise ControlFailure("ACTION_STALE", "message surface target changed since it was indexed")
    geometry = bounds(node)
    root_x, root_y, root_width, root_height = main_geometry(scope)
    if (
        geometry is None or not visible_node(node, (root_x, root_y, root_width, root_height))
        or path_has_editable(locator["path"])
        or geometry[0] + geometry[2] / 2 < root_x + root_width * 0.38
        or geometry[1] < root_y + root_height * 0.12
        or geometry[1] > root_y + root_height * 0.9
    ):
        raise ControlFailure("ACTION_STALE", "message surface target is outside the visible message pane")
    validate_node_signature(node, locator)
    accessible_identity = observable_accessible_identity(
        node, frame["window"].get("atspi_pid"),
    )
    expected_geometry = locator.get("bounds")
    rendered_region = locator.get("rendered_region_sha256")
    if (
        not accessible_identity
        or accessible_identity != locator.get("accessible_identity")
        or not valid_rectangle(expected_geometry)
        or list(geometry) != expected_geometry
        or not isinstance(rendered_region, str)
        or not re.fullmatch(r"[0-9a-f]{64}", rendered_region)
        or screenshot_region_digest(frame, geometry) != rendered_region
    ):
        raise ControlFailure("ACTION_STALE", "message surface observable identity changed")
    action_index = locator.get("action")
    interface = action_interface(node)
    if (
        not isinstance(action_index, int) or interface is None
        or action_index < 0 or action_index >= interface.nActions
        or message_surface_action_index(node) != action_index
    ):
        raise ControlFailure("ACTION_STALE", "message surface action is no longer available")
    return {
        "path": tuple(locator["path"]), "action": action_index,
        "signature": node_signature(node, locator["path"], action_index),
        "accessible_identity": accessible_identity,
        "rendered_region_sha256": rendered_region,
    }


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
    selected_window = verified_window_identity(scope)
    if selected_window.get("process_kind") != "wechat":
        raise ControlFailure("ACTION_STALE", "visible messages require the main WeChat window")
    frame = capture_surface_frame(encode_window_identity(selected_window))
    require_safe_surface_frame(frame)
    require_same_window(scope, frame["scope"])
    verify_conversation_header(title, frame["scope"])
    scope = frame["scope"]
    root_x, root_y, root_width, root_height = main_geometry(scope)
    candidates = []
    seen = set()
    for node, path in frame["nodes"]:
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
        surface_kind = message_surface_kind(node)
        kind = "text"
        if surface_kind == "miniprogram":
            kind = "miniprogram"
        elif surface_kind == "web":
            kind = "link" if role == "link" else "card"
        action_index = message_surface_action_index(node) if surface_kind else None
        surface_locator = ""
        if action_index is not None:
            accessible_identity = observable_accessible_identity(
                node, frame["window"].get("atspi_pid"),
            )
            if accessible_identity:
                surface_locator = make_locator(
                    path, "message_surface", action_index, node=node,
                    source="atspi", bounds_value=geometry, strict_frame=True,
                    rendered_region_sha256=screenshot_region_digest(frame, geometry),
                    accessible_identity_value=accessible_identity,
                    **locator_frame_fields(frame),
                )
        if surface_kind and not surface_locator:
            surface_kind = ""
        candidates.append((y, x, {
            "text": text,
            "kind": kind,
            "sender_name": "",
            "outgoing": outgoing,
            "accessible_label": text if surface_kind else "",
            "surface_kind": surface_kind,
            "surface_locator": surface_locator,
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


def focused_editor_at_path(scope, node_path, expected_signature):
    focused_state = getattr(pyatspi, "STATE_FOCUSED", None)
    if focused_state is None:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "focused accessibility state is unavailable")
    candidates = []
    for candidate, path in visible_scope_nodes(scope):
        if is_editable(candidate) and safe_state(candidate, focused_state):
            candidates.append((candidate, tuple(path)))
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple focused editors are visible")
    if not candidates or candidates[0][1] != tuple(node_path):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "target editor does not own keyboard focus")
    current, current_path = candidates[0]
    if node_signature(current, current_path) != expected_signature:
        raise ControlFailure("ACTION_STALE", "focused editor changed before keyboard input")
    return current


def revalidate_bound_editor(node, node_path, scope):
    node_path = tuple(node_path or ())
    if not node_path or not path_is_within(node_path, scope[1]):
        raise ControlFailure("ACTION_STALE", "editor is outside the bound surface")
    expected_signature = node_signature(node, node_path)
    expected_window = verified_window_identity(scope)
    current_scope = active_surface_root()
    require_same_window(scope, current_scope)
    if verified_window_identity(current_scope) != expected_window:
        raise ControlFailure("ACTION_STALE", "active X11 window changed before text input")
    current = resolve_path(node_path)
    if (
        not is_editable(current)
        or not visible_node(current, main_geometry(current_scope))
        or node_signature(current, node_path) != expected_signature
    ):
        raise ControlFailure("ACTION_STALE", "editor changed before text input")
    return current, current_scope, expected_signature, expected_window


def focus_editor_for_keyboard(node, node_path, scope):
    node, current_scope, expected_signature, before_window = revalidate_bound_editor(
        node, node_path, scope,
    )
    try:
        focused = node.queryComponent().grabFocus()
    except Exception as exc:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "target editor could not receive keyboard focus") from exc
    if not focused:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "target editor rejected keyboard focus")
    current_scope = active_surface_root()
    require_same_window(scope, current_scope)
    if verified_window_identity(current_scope) != before_window:
        raise ControlFailure("ACTION_STALE", "active X11 window changed before keyboard input")
    focused_editor_at_path(current_scope, node_path, expected_signature)
    return current_scope, expected_signature, before_window


def revalidate_keyboard_target(scope, node_path, expected_signature, expected_window):
    current_scope = active_surface_root()
    require_same_window(scope, current_scope)
    if verified_window_identity(current_scope) != expected_window:
        raise ControlFailure("ACTION_STALE", "active X11 window changed before keyboard input")
    focused_editor_at_path(current_scope, node_path, expected_signature)
    return current_scope


def set_text(node, text, scope=None, node_path=None):
    if "\0" in text:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "text contains a NUL byte")
    if scope is None or node_path is None:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "text input needs a bound editor")
    node, scope, _signature, _window = revalidate_bound_editor(node, node_path, scope)
    try:
        if node.queryEditableText().setTextContents(text):
            return
    except Exception:
        pass
    current_scope, expected_signature, expected_window = focus_editor_for_keyboard(
        node, node_path, scope,
    )
    command = ["xclip", "-selection", "clipboard", "-t", "UTF8_STRING", "-i"]
    subprocess.run(command, input=text.encode("utf-8"), check=True)
    try:
        current_scope = revalidate_keyboard_target(
            current_scope, node_path, expected_signature, expected_window,
        )
        send_key("a", control=True)
        current_scope = revalidate_keyboard_target(
            current_scope, node_path, expected_signature, expected_window,
        )
        send_key("v", control=True)
    finally:
        subprocess.run(command, input=b"", check=False)


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


def stage_clipboard_files(paths, editor, editor_path, scope):
    uris = [path.as_uri() for path in validate_attachment_paths(paths)]
    current_scope, expected_signature, expected_window = focus_editor_for_keyboard(
        editor, editor_path, scope,
    )
    subprocess.run(
        ["xclip", "-selection", "clipboard", "-t", "text/uri-list", "-i"],
        input=("\r\n".join(uris) + "\r\n").encode("utf-8"), check=True,
    )
    try:
        revalidate_keyboard_target(
            current_scope, editor_path, expected_signature, expected_window,
        )
        send_key("v", control=True)
    finally:
        subprocess.run(
            ["xclip", "-selection", "clipboard", "-t", "text/uri-list", "-i"],
            input=b"", check=False,
        )


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
    if not isinstance(value, str) or len(value) > 16_384:
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


def node_semantics(node, path, action_index=None, interface=None):
    action = None
    if action_index is not None:
        interface = interface if interface is not None else action_interface(node)
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


def normalize_cjk_spacing(value):
    normalized = unicodedata.normalize("NFKC", str(value or ""))
    # Zero-width, bidi, and other format/control characters are not semantic
    # boundaries. Removing them prevents visually identical safety labels such
    # as D<ZWSP>elete, P<ZWSP>ay, and 删<ZWSP>除 from evading classification.
    normalized = "".join(
        character for character in normalized
        if not default_ignorable_character(character)
    )
    cjk = r"\u3400-\u4dbf\u4e00-\u9fff\uf900-\ufaff"
    return re.sub(r"(?<=[" + cjk + r"])\s+(?=[" + cjk + r"])", "", normalized)


def default_ignorable_character(character):
    codepoint = ord(character)
    if unicodedata.category(character) in ("Cc", "Cf", "Cs"):
        return True
    return (
        codepoint == 0x034F
        or 0x115F <= codepoint <= 0x1160
        or 0x17B4 <= codepoint <= 0x17B5
        or 0x180B <= codepoint <= 0x180F
        or codepoint == 0x3164
        or 0xFE00 <= codepoint <= 0xFE0F
        or codepoint == 0xFFA0
        or 0xFFF0 <= codepoint <= 0xFFF8
        or 0xE0100 <= codepoint <= 0xE01EF
    )


def normalized_exact(value):
    normalized = normalize_cjk_spacing(value)
    return " ".join(normalized.split()).casefold()


def validated_surface_name(value):
    raw = str(value or "")
    if any(unicodedata.category(character) in ("Cc", "Cf") for character in raw):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface name contains control or formatting characters")
    normalized = unicodedata.normalize("NFKC", raw)
    normalized = " ".join(normalized.split())
    if not normalized or len(normalized) > 128 or "\0" in normalized:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface name must contain 1 to 128 visible characters")
    return normalized


def capture_screenshot_png(window_id):
    environment = os.environ.copy()
    environment["DISPLAY"] = environment.get("WECHAT_DISPLAY", environment.get("DISPLAY", ":99"))
    completed = subprocess.run(
        ["import", "-silent", "-display", environment["DISPLAY"], "-window", str(window_id), "png:-"],
        check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        env=environment, timeout=15,
    )
    screenshot = completed.stdout
    if (
        not screenshot.startswith(b"\x89PNG\r\n\x1a\n")
        or len(screenshot) > MAX_SCREENSHOT_BYTES
    ):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "desktop helper returned an invalid screenshot")
    return screenshot


def screenshot_rgba(screenshot):
    if len(screenshot) < 24 or screenshot[:8] != b"\x89PNG\r\n\x1a\n" or screenshot[12:16] != b"IHDR":
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface screenshot has no valid PNG header")
    width = int.from_bytes(screenshot[16:20], "big")
    height = int.from_bytes(screenshot[20:24], "big")
    if (
        width <= 0 or height <= 0 or width > 16_384 or height > 16_384
        or width * height > MAX_SCREENSHOT_PIXELS
    ):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface screenshot dimensions are outside the safe limit")
    try:
        completed = subprocess.run(
            ["convert", "png:-", "-alpha", "on", "-depth", "8", "rgba:-"],
            input=screenshot, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            timeout=15,
        )
    except (FileNotFoundError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface screenshot pixels could not be decoded") from exc
    pixels = completed.stdout
    expected = width * height * 4
    if len(pixels) != expected:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface screenshot pixel buffer is inconsistent")
    return pixels, (width, height)


def screenshot_region_digest(frame, absolute_bounds, margin=OCR_CONTEXT_MARGIN):
    pixels = frame.get("screenshot_rgba")
    dimensions = frame.get("screenshot_dimensions")
    if not isinstance(pixels, bytes) or not isinstance(dimensions, tuple) or len(dimensions) != 2:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface snapshot has no decoded pixel buffer")
    image_width, image_height = dimensions
    origin_x, origin_y = frame["window"]["geometry"][:2]
    x, y, width, height = absolute_bounds
    left = max(0, x - origin_x - margin)
    top = max(0, y - origin_y - margin)
    right = min(image_width, x - origin_x + width + margin)
    bottom = min(image_height, y - origin_y + height + margin)
    if right <= left or bottom <= top:
        raise ControlFailure("ACTION_STALE", "visual target is outside the bound screenshot")
    digest = hashlib.sha256()
    digest.update(f"{left}:{top}:{right}:{bottom}".encode("ascii"))
    stride = image_width * 4
    for row in range(top, bottom):
        start = row * stride + left * 4
        digest.update(pixels[start:start + (right - left) * 4])
    return digest.hexdigest()


def rendered_frame_digest(pixels, dimensions):
    if (
        not isinstance(pixels, bytes) or not isinstance(dimensions, tuple)
        or len(dimensions) != 2
        or not all(isinstance(value, int) and value > 0 for value in dimensions)
        or len(pixels) != dimensions[0] * dimensions[1] * 4
    ):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface frame pixels are inconsistent")
    digest = hashlib.sha256()
    digest.update(f"rgba8:{dimensions[0]}:{dimensions[1]}\0".encode("ascii"))
    digest.update(pixels)
    return digest.hexdigest()


def require_screenshot_geometry(window_identity, dimensions):
    expected = tuple(window_identity["geometry"][2:])
    if tuple(dimensions) != expected:
        raise ControlFailure(
            "CLIENT_INCOMPATIBLE",
            "surface screenshot dimensions do not match the bound X11 window",
        )


def parse_tesseract_tsv(payload):
    try:
        lines = payload.decode("utf-8", "replace").splitlines()
    except AttributeError:
        lines = str(payload).splitlines()
    words = []
    for line in lines[1:]:
        columns = line.split("\t", 11)
        if len(columns) != 12 or columns[0] != "5":
            continue
        text = " ".join(columns[11].split())
        if not text:
            continue
        try:
            confidence = float(columns[10])
            x, y, width, height = (int(columns[index]) for index in range(6, 10))
        except ValueError:
            continue
        if confidence < 30 or width <= 1 or height <= 1:
            continue
        words.append({
            "line": tuple(columns[index] for index in (1, 2, 3, 4)),
            "text": text,
            "bounds": (x, y, width, height),
            "confidence": min(0.99, max(0.3, confidence / 100.0)),
        })

    grouped = []
    for key in dict.fromkeys(item["line"] for item in words):
        row = [item for item in words if item["line"] == key]
        row.sort(key=lambda item: item["bounds"][0])
        x1 = min(item["bounds"][0] for item in row)
        y1 = min(item["bounds"][1] for item in row)
        x2 = max(item["bounds"][0] + item["bounds"][2] for item in row)
        y2 = max(item["bounds"][1] + item["bounds"][3] for item in row)
        # Tesseract inserts spaces between separately recognized CJK words.
        # Keeping them is useful for prose; exact matching ignores whitespace.
        grouped.append({
            "text": " ".join(item["text"] for item in row),
            "bounds": (x1, y1, x2 - x1, y2 - y1),
            "confidence": sum(item["confidence"] for item in row) / len(row),
        })
    return grouped


def tesseract_regions(screenshot):
    try:
        completed = subprocess.run(
            ["tesseract", "stdin", "stdout", "-l", "chi_sim+eng", "--psm", "11", "tsv"],
            input=screenshot, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            timeout=30,
        )
    except (FileNotFoundError, subprocess.CalledProcessError, subprocess.TimeoutExpired):
        return []
    return parse_tesseract_tsv(completed.stdout)


def visible_scope_nodes(scope):
    geometry = main_geometry(scope)
    return [(node, path) for node, path in scope_nodes(scope) if visible_node(node, geometry)]


def helper_output(arguments):
    environment = os.environ.copy()
    environment["DISPLAY"] = environment.get("WECHAT_DISPLAY", environment.get("DISPLAY", ":99"))
    environment["LC_ALL"] = "C"
    try:
        completed = subprocess.run(
            arguments, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
            env=environment, timeout=5, text=True,
        )
    except (FileNotFoundError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as exc:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 window identity is unavailable") from exc
    return completed.stdout


def process_record(pid):
    if not isinstance(pid, int) or pid <= 1:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "active window has no valid process identity")
    try:
        stat = (Path("/proc") / str(pid) / "stat").read_text(encoding="utf-8")
        closing = stat.rfind(")")
        fields = stat[closing + 2:].split()
        parent_pid = int(fields[1])
        starttime = int(fields[19])
        cmdline = (Path("/proc") / str(pid) / "cmdline").read_bytes().split(b"\0")
        names = [value.rsplit(b"/", 1)[-1].decode("utf-8", "replace").lower() for value in cmdline if value]
        try:
            executable = os.path.basename(os.readlink(Path("/proc") / str(pid) / "exe")).lower()
        except OSError:
            executable = ""
    except (OSError, ValueError, IndexError) as exc:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "active window process identity changed") from exc
    return {
        "pid": pid,
        "parent_pid": parent_pid,
        "starttime": starttime,
        "names": names,
        "executable": executable,
    }


def process_lineage(pid):
    result = []
    seen = set()
    for _depth in range(8):
        if pid in seen or pid <= 1:
            break
        seen.add(pid)
        record = process_record(pid)
        result.append(record)
        pid = record["parent_pid"]
    return result


def x11_active_window():
    root_properties = helper_output(["xprop", "-root", "_NET_ACTIVE_WINDOW"])
    match = re.search(r"\b0x[0-9a-fA-F]+\b", root_properties)
    if match is None or int(match.group(0), 16) == 0:
        raise ControlFailure("SURFACE_MISSING", "no active X11 window is available")
    xid = "0x" + format(int(match.group(0), 16), "x")
    properties = helper_output(["xprop", "-id", xid, "_NET_WM_PID", "WM_CLASS"])
    pid_match = re.search(r"_NET_WM_PID\([^)]*\)\s*=\s*(\d+)", properties)
    if pid_match is None:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "active window has no process owner")
    class_line = next((line for line in properties.splitlines() if line.startswith("WM_CLASS")), "")
    wm_class = [value.lower() for value in re.findall(r'"([^"]+)"', class_line)]
    geometry_text = helper_output(["xwininfo", "-id", xid])

    def geometry_value(label):
        match_value = re.search(r"^\s*" + re.escape(label) + r":\s*(-?\d+)\s*$", geometry_text, re.MULTILINE)
        if match_value is None:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "active window geometry is unavailable")
        return int(match_value.group(1))

    geometry = (
        geometry_value("Absolute upper-left X"),
        geometry_value("Absolute upper-left Y"),
        geometry_value("Width"),
        geometry_value("Height"),
    )
    return xid, int(pid_match.group(1)), wm_class, geometry


def x11_wechat_window_inventory():
    listing = helper_output(["xprop", "-root", "_NET_CLIENT_LIST_STACKING"])
    result = set()
    for raw_xid in re.findall(r"\b0x[0-9a-fA-F]+\b", listing):
        xid = "0x" + format(int(raw_xid, 16), "x")
        try:
            properties = helper_output(["xprop", "-id", xid, "_NET_WM_PID", "WM_CLASS"])
            pid_match = re.search(r"_NET_WM_PID\([^)]*\)\s*=\s*(\d+)", properties)
            if pid_match is None:
                continue
            pid = int(pid_match.group(1))
            wm_class = [value.lower() for value in re.findall(r'"([^"]+)"', properties)]
            lineage = process_lineage(pid)
            evidence = " ".join(
                wm_class + [
                    value for record in lineage
                    for value in record["names"] + [record["executable"]] if value
                ]
            )
            if not any(marker in evidence for marker in ("wechat", "xwechat", "wmpf", "wechatappex", "xweb")):
                continue
            kind = "miniprogram" if any(
                marker in evidence for marker in ("wmpf", "wechatappex", "mini-program")
            ) else ("web" if "xweb" in evidence else "wechat")
            result.add((xid, pid, lineage[0]["starttime"], kind))
        except ControlFailure:
            continue
    return result


def focus_x11_window(xid):
    if not isinstance(xid, str) or not re.fullmatch(r"0x[0-9a-f]+", xid):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "main WeChat window identifier is invalid")
    from Xlib import X, display
    from Xlib.protocol import event as xevent
    connection = display.Display()
    try:
        root = connection.screen().root
        active_window = connection.intern_atom("_NET_ACTIVE_WINDOW")
        event = xevent.ClientMessage(
            window=int(xid, 16),
            client_type=active_window,
            data=(32, [1, X.CurrentTime, 0, 0, 0]),
        )
        root.send_event(
            event,
            event_mask=X.SubstructureRedirectMask | X.SubstructureNotifyMask,
        )
        connection.sync()
    finally:
        connection.close()


def close_x11_window(xid):
    if not isinstance(xid, str) or not re.fullmatch(r"0x[0-9a-f]+", xid):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface window identifier is invalid")
    from Xlib import X, display
    from Xlib.protocol import event as xevent
    connection = display.Display()
    try:
        root = connection.screen().root
        close_window = connection.intern_atom("_NET_CLOSE_WINDOW")
        event = xevent.ClientMessage(
            window=int(xid, 16),
            client_type=close_window,
            data=(32, [X.CurrentTime, 1, 0, 0, 0]),
        )
        root.send_event(
            event,
            event_mask=X.SubstructureRedirectMask | X.SubstructureNotifyMask,
        )
        connection.sync()
    finally:
        connection.close()


def focus_unique_main_wechat_window():
    main_windows = [
        item for item in x11_wechat_window_inventory() if item[3] == "wechat"
    ]
    if len(main_windows) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple official main WeChat windows are available")
    if not main_windows:
        raise ControlFailure("SURFACE_MISSING", "the official main WeChat window is not available")
    expected_xid = main_windows[0][0]
    focus_x11_window(expected_xid)
    deadline = time.monotonic() + MAIN_WINDOW_FOCUS_TIMEOUT_SECONDS
    last_error = None
    while time.monotonic() < deadline:
        try:
            if x11_active_window()[0] != expected_xid:
                time.sleep(0.05)
                continue
            frame = capture_surface_frame()
            if frame["window"].get("process_kind") != "wechat":
                raise ControlFailure("ACTION_STALE", "focused window is not the official main WeChat window")
            return frame
        except ControlFailure as exc:
            last_error = exc
            time.sleep(0.05)
    if last_error is not None:
        raise last_error
    raise ControlFailure("ACTION_STALE", "official main WeChat window did not receive focus")


def main_wechat_frame_for_navigation():
    frame = capture_surface_frame()
    require_safe_surface_frame(frame)
    kind = frame["window"].get("process_kind")
    if kind == "wechat":
        return frame
    if kind not in ("miniprogram", "web"):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "active window is not in the official WeChat process family")
    # Daemon restart loses in-memory surface sessions, but the official client
    # can retain an active WMPF/XWeb window. Focusing the unique existing main
    # window is reversible and preserves any uncommitted state in that surface.
    frame = focus_unique_main_wechat_window()
    require_safe_surface_frame(frame)
    return frame


def atspi_owner(scope):
    root = scope[0]
    candidates = [root]
    try:
        application = root.getApplication()
        if application is not None:
            candidates.append(application)
    except Exception:
        pass
    process_ids = set()
    xids = set()
    for candidate in candidates:
        try:
            process_id = int(candidate.get_process_id())
            if process_id > 1:
                process_ids.add(process_id)
        except Exception:
            pass
        try:
            attributes = candidate.getAttributes()
        except Exception:
            attributes = []
        for attribute in attributes or []:
            key, separator, value = str(attribute).partition(":")
            if not separator or key.strip().lower() not in ("xid", "window-id", "window_id"):
                continue
            try:
                parsed = int(value.strip(), 0)
            except ValueError:
                continue
            if parsed > 0:
                xids.add("0x" + format(parsed, "x"))
    if len(process_ids) != 1 or len(xids) > 1:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "accessibility window owner is unavailable or ambiguous")
    return next(iter(process_ids)), (next(iter(xids)) if xids else "")


def matching_window_geometry(first, second):
    first_x, first_y, first_width, first_height = first
    second_x, second_y, second_width, second_height = second
    return (
        abs(first_x - second_x) <= 32
        and abs(first_y - second_y) <= 48
        and abs(first_width - second_width) <= 32
        and abs(first_height - second_height) <= 48
    )


def verified_window_identity(scope):
    xid, pid, wm_class, window_geometry = x11_active_window()
    lineage = process_lineage(pid)
    process_names = [
        value
        for record in lineage
        for value in record["names"] + [record["executable"]]
        if value
    ]
    evidence = " ".join(wm_class + process_names)
    mini = any(marker in evidence for marker in ("wmpf", "wechatappex", "mini-program"))
    web = "xweb" in evidence and not mini
    wechat = mini or web or any(marker in evidence for marker in ("wechat", "xwechat", "微信"))
    if not wechat:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "active window is not owned by the official WeChat process family")
    scope_geometry = main_geometry(scope)
    if not matching_window_geometry(window_geometry, scope_geometry):
        raise ControlFailure("ACTION_STALE", "X11 and accessibility window geometry do not identify the same surface")
    atspi_pid, atspi_xid = atspi_owner(scope)
    lineage_pids = {record["pid"] for record in lineage}
    if atspi_pid not in lineage_pids:
        raise ControlFailure("ACTION_STALE", "X11 and accessibility windows have different process owners")
    if atspi_xid and atspi_xid != xid:
        raise ControlFailure("ACTION_STALE", "X11 and accessibility windows have different window identifiers")
    value = {
        "version": 1,
        "xid": xid,
        "pid": pid,
        "pid_starttime": lineage[0]["starttime"],
        "process_kind": "miniprogram" if mini else ("web" if web else "wechat"),
        "atspi_pid": atspi_pid,
        "atspi_xid": atspi_xid,
        "lineage": [
            {"pid": record["pid"], "starttime": record["starttime"]}
            for record in lineage
        ],
        "wm_class": wm_class,
        "geometry": list(window_geometry),
        "scope_geometry": list(scope_geometry),
    }
    value["digest"] = semantic_digest(value)
    return value


def encode_window_identity(identity):
    payload = json.dumps(identity, sort_keys=True, separators=(",", ":")).encode("utf-8")
    return base64.urlsafe_b64encode(payload).rstrip(b"=").decode("ascii")


def decode_window_identity(value):
    if not isinstance(value, str) or not value or len(value) > 4_096:
        raise ControlFailure("ACTION_STALE", "surface window identity is missing")
    try:
        padding = "=" * (-len(value) % 4)
        identity = json.loads(base64.urlsafe_b64decode(value + padding))
    except (ValueError, TypeError, json.JSONDecodeError) as exc:
        raise ControlFailure("ACTION_STALE", "surface window identity is invalid") from exc
    if not isinstance(identity, dict) or identity.get("version") != 1:
        raise ControlFailure("ACTION_STALE", "surface window identity is invalid")
    supplied_digest = identity.pop("digest", None)
    expected_digest = semantic_digest(identity)
    identity["digest"] = supplied_digest
    if not isinstance(supplied_digest, str) or supplied_digest != expected_digest:
        raise ControlFailure("ACTION_STALE", "surface window identity is invalid")
    return identity


def require_window_identity(expected, actual):
    if expected is None:
        return
    decoded = decode_window_identity(expected)
    if decoded != actual:
        raise ControlFailure("ACTION_STALE", "active window is not the bound surface window")


def surface_semantic_fingerprint(root, root_path, nodes):
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


def surface_capture_fingerprint(root, root_path, nodes):
    stable_nodes = []
    for node, path in nodes:
        interface = action_interface(node)
        role = safe_role(node)
        if interface is not None:
            for index in range(min(interface.nActions, 8)):
                stable_nodes.append(node_semantics(node, path, index))
        elif is_editable(node) or role in IMAGE_ROLES:
            stable_nodes.append(node_semantics(node, path))
    return semantic_digest({
        "root": {
            "path": list(root_path), "role": safe_role(root),
            "bounds": list(bounds(root) or ()),
        },
        "interactive_and_images": stable_nodes,
    })


def frame_semantic_generation(root, root_path, nodes, ocr=()):
    return semantic_digest({
        "atspi": surface_semantic_fingerprint(root, root_path, nodes),
        "ocr": [
            {"text": normalized_exact(item["text"]), "bounds": list(item["bounds"])}
            for item in ocr
        ],
    })


def capture_surface_frame(expected_window_identity=None):
    before_scope = active_surface_root()
    before_window = verified_window_identity(before_scope)
    require_window_identity(expected_window_identity, before_window)
    before_nodes = visible_scope_nodes(before_scope)
    before_semantic = surface_capture_fingerprint(
        before_scope[0], before_scope[1], before_nodes,
    )
    screenshot = capture_screenshot_png(before_window["xid"])
    screenshot_sha256 = hashlib.sha256(screenshot).hexdigest()
    pixels, screenshot_dimensions = screenshot_rgba(screenshot)
    require_screenshot_geometry(before_window, screenshot_dimensions)
    ocr = tesseract_regions(screenshot)
    after_scope = active_surface_root()
    require_same_window(before_scope, after_scope)
    after_window = verified_window_identity(after_scope)
    if before_window != after_window:
        raise ControlFailure("ACTION_STALE", "active X11 window changed while its snapshot was captured")
    after_nodes = visible_scope_nodes(after_scope)
    after_semantic = surface_capture_fingerprint(
        after_scope[0], after_scope[1], after_nodes,
    )
    if before_semantic != after_semantic:
        raise ControlFailure("ACTION_STALE", "surface changed while its snapshot was captured")
    result = {
        "scope": after_scope,
        "nodes": after_nodes,
        "screenshot": screenshot,
        "screenshot_rgba": pixels,
        "screenshot_dimensions": screenshot_dimensions,
        "screenshot_sha256": screenshot_sha256,
        "rendered_frame_sha256": rendered_frame_digest(pixels, screenshot_dimensions),
        "ocr": [],
        "window": after_window,
        "window_identity": encode_window_identity(after_window),
    }
    offset_x, offset_y = after_window["geometry"][:2]
    for item in ocr:
        x, y, width, height = item["bounds"]
        absolute = dict(item)
        absolute["bounds"] = (x + offset_x, y + offset_y, width, height)
        if geometry_contains(main_geometry(after_scope), absolute["bounds"]):
            result["ocr"].append(absolute)
    result["generation"] = frame_semantic_generation(
        after_scope[0], after_scope[1], after_nodes, result["ocr"],
    )
    return result


def revalidate_visual_region(
    expected_window_identity, region, expected_signature,
    expected_rendered_frame_sha256, expected_generation, action_kind="visual_activate",
):
    if (
        not isinstance(expected_rendered_frame_sha256, str)
        or not re.fullmatch(r"[0-9a-f]{64}", expected_rendered_frame_sha256)
        or not isinstance(expected_generation, str)
        or not re.fullmatch(r"[0-9a-f]{64}", expected_generation)
    ):
        raise ControlFailure("ACTION_STALE", "visual action has no complete frame binding")
    # Re-run the complete stable-frame sandwich, including OCR. The old local
    # crop check passed an empty OCR list to the security gate and therefore
    # could miss a newly rendered permission/payment/challenge prompt.
    frame = capture_surface_frame(expected_window_identity)
    require_safe_surface_frame(frame)
    if any(
        rectangles_overlap(region["bounds"], item["bounds"], threshold=0.6)
        for item in high_risk_visual_regions(frame)
    ):
        raise ControlFailure("USER_ACTION_REQUIRED", "rendered high-risk action requires direct user interaction")
    if high_risk_surface_context(frame) and not high_risk_context_allows(
        action_kind, region["text"],
    ):
        raise ControlFailure("USER_ACTION_REQUIRED", "generic actions are disabled on a high-risk surface")
    if (
        frame["rendered_frame_sha256"] != expected_rendered_frame_sha256
        or frame["generation"] != expected_generation
    ):
        raise ControlFailure("ACTION_STALE", "complete rendered surface changed before visual activation")
    matches = [
        item for item in frame["ocr"]
        if tuple(item["bounds"]) == tuple(region["bounds"])
        and normalized_exact(item["text"]) == normalized_exact(region["text"])
    ]
    if len(matches) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "visual target became ambiguous before activation")
    if len(matches) != 1 or ocr_region_signature(frame, matches[0]) != expected_signature:
        raise ControlFailure("ACTION_STALE", "visual target changed before activation")
    return frame


def frame_text(frame):
    atspi = collect_strings(frame["nodes"], limit=500)
    ocr = [item["text"] for item in frame["ocr"]]
    return normalize_cjk_spacing("\n".join(atspi + ocr)).lower()


def require_safe_surface_frame(frame):
    if (
        frame.get("window", {}).get("process_kind") == "wechat"
        and authentication_context(frame["nodes"])
    ):
        raise ControlFailure("AUTH_REQUIRED", "authentication must be completed in the official WeChat client")
    if security_challenge_context(frame):
        raise ControlFailure("USER_ACTION_REQUIRED", "WeChat security verification requires direct user interaction")


def security_challenge_context(frame):
    _root_x, root_y, _root_width, root_height = main_geometry(frame["scope"])
    root_path = tuple(frame["scope"][1])
    marker_values = {normalized_exact(marker) for marker in SECURITY_CHALLENGE_MARKERS}
    has_control = any(
        action_interface(node) is not None or is_editable(node)
        for node, _path in frame["nodes"]
    )
    for node, path in frame["nodes"]:
        geometry = bounds(node)
        if geometry is None:
            continue
        text = normalized_exact(" ".join((safe_name(node), safe_description(node))))
        matching = [marker for marker in marker_values if marker and marker in text]
        if not matching:
            continue
        compact = any(text == marker or len(text) <= len(marker) + 8 for marker in matching)
        near_top = geometry[1] <= root_y + root_height * 0.45
        if action_interface(node) is not None or is_editable(node):
            return True
        if (
            has_control and compact and near_top
            and safe_role(node) in ("heading", "title bar", "dialog", "alert", "label", "text")
            and len(tuple(path)) <= len(root_path) + 3
        ):
            return True
    ocr_markers = []
    marker_hits = set()
    ocr_controls = []
    control_values = {
        normalized_exact(marker)
        for marker in CONFIRM_LABELS + (
            "继续", "开始验证", "开始", "verify", "start verification",
            "滑块", "slider",
        )
    }
    for item in frame["ocr"]:
        text = normalized_exact(item["text"])
        matching = [marker for marker in marker_values if marker and marker in text]
        if (
            item["bounds"][1] <= root_y + root_height * 0.55
            and any(text == marker or len(text) <= len(marker) + 6 for marker in matching)
        ):
            ocr_markers.append(item)
            marker_hits.update(matching)
        if any(
            text == marker or (marker in text and len(text) <= len(marker) + 4)
            for marker in control_values if marker
        ):
            ocr_controls.append(item)
    if len(marker_hits) >= 2 and len(ocr_markers) >= 2:
        return True
    return any(
        control["bounds"][1] > marker["bounds"][1]
        for marker in ocr_markers for control in ocr_controls
        if control is not marker
    )


def high_risk_surface_context(frame):
    root_x, root_y, root_width, root_height = main_geometry(frame["scope"])
    marker_values = {normalized_exact(marker) for marker in HIGH_RISK_MARKERS}
    root_path = tuple(frame["scope"][1])
    for node, path in frame["nodes"]:
        geometry = bounds(node)
        if geometry is None:
            continue
        text = normalized_exact(" ".join((safe_name(node), safe_description(node))))
        if not text:
            continue
        matching = [marker for marker in marker_values if marker and marker in text]
        if not matching:
            continue
        role = safe_role(node)
        near_top = geometry[1] <= root_y + root_height * 0.3
        compact = any(text == marker or len(text) <= len(marker) + 8 for marker in matching)
        if action_interface(node) is not None or is_editable(node):
            return True
        if (
            near_top and compact
            and role in ("heading", "title bar", "dialog", "alert", "label", "text")
            and len(tuple(path)) <= len(root_path) + 3
        ):
            return True
    ocr_risk = []
    ocr_confirm = []
    confirm_values = {
        normalized_exact(marker) for marker in CONFIRM_LABELS + ("继续", "pay now")
    }
    for item in frame["ocr"]:
        text = normalized_exact(item["text"])
        y = item["bounds"][1]
        matching = [marker for marker in marker_values if marker and marker in text]
        if (
            y <= root_y + root_height * 0.35
            and any(text == marker or len(text) <= len(marker) + 5 for marker in matching)
        ):
            ocr_risk.append(item)
        if any(text == marker or (marker in text and len(text) <= len(marker) + 4) for marker in confirm_values):
            ocr_confirm.append(item)
    if any(
        confirmation["bounds"][1] > risk["bounds"][1]
        for risk in ocr_risk for confirmation in ocr_confirm
    ):
        return True
    return False


def high_risk_context_allows(kind, label):
    return kind in ("scroll", "back") or (
        kind == "visual_activate" and has_semantic_marker(label, CLOSE_LABELS)
    )


def node_signature(node, path, action_index=None, interface=None):
    return semantic_digest(node_semantics(node, path, action_index, interface=interface))


def node_replay_identity(node, path, action_index=None):
    interface = action_interface(node) if action_index is not None else None
    action = ""
    if action_index is not None:
        if interface is None or action_index < 0 or action_index >= interface.nActions:
            raise ControlFailure("ACTION_STALE", "surface action is no longer available")
        action = action_name(interface, action_index)
    # Replay identity is deliberately independent of the transient AT-SPI
    # path, bounds, action index, window XID and current editor contents. Those
    # values all belong in the one-frame locator/action ID, but allowing any of
    # them to identify a logical mutation would let a reflow or a newly opened
    # window resurrect an already-dispatched write/unknown action. Collisions
    # between indistinguishable controls fail closed by tombstoning both.
    return semantic_digest({
        "name": normalized_exact(safe_name(node)),
        "description": normalized_exact(safe_description(node)),
        "role": normalized_exact(safe_role(node)),
        "action": normalized_exact(action),
        "editable": is_editable(node),
    })


def validate_node_signature(node, locator, interface=None):
    if locator.get("version") not in (2, 3, 4) or not isinstance(locator.get("node_signature"), str):
        raise ControlFailure("ACTION_STALE", "locator predates the current accessibility snapshot")
    action_index = locator.get("action")
    if action_index is not None and not isinstance(action_index, int):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid locator action")
    if node_signature(
        node, locator["path"], action_index, interface=interface,
    ) != locator["node_signature"]:
        raise ControlFailure("ACTION_STALE", "semantic target changed since it was observed")


def surface_generation(root, root_path, nodes, screenshot_sha256=""):
    # Kept as a test/helper API. Pixel freshness is target-local for OCR and
    # must not invalidate unrelated AT-SPI controls.
    return frame_semantic_generation(root, root_path, nodes)


def surface_kind_candidates(nodes, scope_geometry, kind):
    candidates = []
    for node, path in nodes:
        if not visible_node(node, scope_geometry):
            continue
        label = safe_name(node)
        description = safe_description(node)
        role = safe_role(node)
        interface = action_interface(node)
        if interface is None:
            continue
        for index in range(min(interface.nActions, 8)):
            candidate_kind, _risk = classify_action(
                label, role, action_name(interface, index),
                description=description, editable=is_editable(node),
            )
            if candidate_kind == kind:
                candidates.append((node, path, index))
    return candidates


def valid_rectangle(value):
    return (
        isinstance(value, list) and len(value) == 4
        and all(isinstance(item, int) for item in value)
        and value[2] > 0 and value[3] > 0
    )


def ocr_action_kind(text):
    return "input" if has_semantic_marker(text, SEARCH_MARKERS) else "visual_activate"


def classify_ocr_action_effect(text):
    """Classify a coordinate-only OCR control without trusting its appearance.

    A visual label has no accessibility role or action contract. It is only
    executable when its text positively identifies a search field or read-only
    navigation. Known writes still use the explicit confirmation path; an
    otherwise unknown visual click is permanently disabled because it could be
    a purchase/permission control whose wording we have not enumerated.
    """
    kind = ocr_action_kind(text)
    _semantic_kind, risk, effect = classify_action_effect(
        text, "ocr text", "visual activate",
    )
    if risk == "high":
        return kind, "high", "high_risk"
    if effect == "external_write":
        return kind, "medium", "external_write"
    if kind == "input":
        return kind, "low", "search_input"
    if effect == "navigate":
        return kind, "low", "navigate"
    return kind, "high", "high_risk"


def ocr_region_signature(frame, region):
    return semantic_digest({
        "pixels": screenshot_region_digest(frame, region["bounds"]),
        "text": normalized_exact(region["text"]),
        "bounds": list(region["bounds"]),
    })


def validate_surface_locator(
    locator, expected_kind=None, require_unique_kind=False, return_action_interface=False,
):
    kind = locator.get("kind")
    if expected_kind is not None and kind != expected_kind:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface locator has the wrong semantic kind")
    generation = locator.get("surface_generation")
    screenshot_sha256 = locator.get("screenshot_sha256")
    if (
        locator.get("version") != 4
        or not isinstance(generation, str)
        or not isinstance(screenshot_sha256, str)
    ):
        raise ControlFailure("ACTION_STALE", "surface locator predates the current snapshot")
    expires = locator.get("expires_unix")
    issued = locator.get("issued_unix")
    if (
        not isinstance(issued, int) or not isinstance(expires, int)
        or expires <= issued or expires - issued > SURFACE_LOCATOR_TTL_SECONDS
        or time.time() > expires
    ):
        raise ControlFailure("ACTION_STALE", "surface locator has expired")
    frame = capture_surface_frame(locator.get("window_identity"))
    require_safe_surface_frame(frame)
    strict_frame = locator.get("strict_frame")
    if not isinstance(strict_frame, bool):
        raise ControlFailure("ACTION_STALE", "surface locator has an invalid frame policy")
    rendered_frame_sha256 = locator.get("rendered_frame_sha256")
    if not isinstance(rendered_frame_sha256, str) or not re.fullmatch(r"[0-9a-f]{64}", rendered_frame_sha256):
        raise ControlFailure("ACTION_STALE", "surface locator has no stable rendered-frame identity")
    if strict_frame and (
        frame.get("generation") != generation
        or frame.get("rendered_frame_sha256") != rendered_frame_sha256
    ):
        raise ControlFailure(
            "ACTION_STALE",
            "surface content changed after this sensitive action was observed",
        )
    high_risk_context = high_risk_surface_context(frame)
    scope = frame["scope"]
    root, root_path = scope
    if locator.get("scope_identity") != scope_identity(scope):
        raise ControlFailure("ACTION_STALE", "surface locator belongs to a different window")
    if locator.get("scope_geometry") != list(main_geometry(scope)):
        raise ControlFailure("ACTION_STALE", "surface window geometry changed")
    if not path_is_within(locator["path"], root_path):
        raise ControlFailure("ACTION_STALE", "surface locator belongs to a different window")
    nodes = frame["nodes"]
    node = resolve_path(locator["path"])
    if not visible_node(node, main_geometry(scope)):
        raise ControlFailure("ACTION_STALE", "surface target is no longer visible")

    source = locator.get("source", "atspi")
    if kind == "scroll":
        if (
            source != "viewport" or tuple(locator["path"]) != tuple(root_path)
            or locator.get("direction") not in ("up", "down")
        ):
            raise ControlFailure("ACTION_STALE", "viewport scroll locator is invalid")
        return (node, scope, None) if return_action_interface else (node, scope)

    if source == "ocr":
        if not strict_frame:
            raise ControlFailure("ACTION_STALE", "visual actions require an exact rendered frame")
        expected_bounds = locator.get("bounds")
        expected_text = locator.get("text")
        if not valid_rectangle(expected_bounds) or not isinstance(expected_text, str):
            raise ControlFailure("ACTION_STALE", "visual action locator is invalid")
        matches = [
            region for region in frame["ocr"]
            if list(region["bounds"]) == expected_bounds
            and normalized_exact(region["text"]) == normalized_exact(expected_text)
        ]
        if len(matches) > 1:
            raise ControlFailure("TARGET_AMBIGUOUS", "visual target is no longer unique")
        if len(matches) != 1:
            raise ControlFailure("ACTION_STALE", "visual target changed since it was observed")
        region = matches[0]
        if kind == "input":
            search_regions = [
                item for item in frame["ocr"]
                if ocr_action_kind(item["text"]) == "input"
            ]
            if len(search_regions) > 1:
                raise ControlFailure("TARGET_AMBIGUOUS", "multiple visual search inputs are visible")
            if len(search_regions) != 1 or search_regions[0] is not region:
                raise ControlFailure("ACTION_STALE", "visual search input is no longer unique")
        if (
            ocr_action_kind(region["text"]) != kind
            or ocr_region_signature(frame, region) != locator.get("region_signature")
        ):
            raise ControlFailure("ACTION_STALE", "visual target semantics changed")
        live_kind, live_risk, _effect = classify_ocr_action_effect(region["text"])
        if live_kind != kind:
            raise ControlFailure("ACTION_STALE", "visual target action semantics changed")
        if live_risk == "high":
            raise ControlFailure("USER_ACTION_REQUIRED", "high-risk visual action requires direct user interaction")
        if high_risk_context and not high_risk_context_allows(kind, region["text"]):
            raise ControlFailure("USER_ACTION_REQUIRED", "generic actions are disabled on a high-risk surface")
        return (node, scope, None) if return_action_interface else (node, scope)

    if source != "atspi":
        raise ControlFailure("CLIENT_INCOMPATIBLE", "unknown surface locator source")
    action_index = locator.get("action")
    validated_interface = action_interface(node) if action_index is not None else None
    if action_index is not None and validated_interface is None:
        raise ControlFailure("ACTION_STALE", "surface action is no longer available")
    validate_node_signature(node, locator, interface=validated_interface)
    node_geometry = bounds(node)
    if node_geometry is not None and any(
        rectangles_overlap(node_geometry, region["bounds"], threshold=0.6)
        for region in high_risk_visual_regions(frame)
    ):
        raise ControlFailure(
            "USER_ACTION_REQUIRED",
            "rendered high-risk action requires direct user interaction",
        )
    if high_risk_context and not high_risk_context_allows(
        kind, safe_name(node) or safe_description(node),
    ):
        raise ControlFailure("USER_ACTION_REQUIRED", "generic actions are disabled on a high-risk surface")
    search_paths = [
        tuple(path) for candidate, path in nodes
        if is_editable(candidate) and has_semantic_marker(
            safe_name(candidate) + " " + safe_description(candidate), SEARCH_MARKERS,
        )
    ]
    unique_search = len(search_paths) == 1 and tuple(locator["path"]) == search_paths[0]
    if action_index is None:
        if kind != "input" or not is_editable(node):
            raise ControlFailure("ACTION_STALE", "surface input no longer matches the locator")
        _kind, live_risk, live_effect = classify_action_effect(
            safe_name(node), safe_role(node), "", description=safe_description(node),
            editable=True, unique_search_input=unique_search,
        )
        if (live_risk != "low" or live_effect in ("external_write", "high_risk")) and not strict_frame:
            raise ControlFailure("ACTION_STALE", "sensitive input locator has no exact frame binding")
    else:
        live_kind, live_risk, live_effect = classify_action_effect(
            safe_name(node), safe_role(node), action_name(validated_interface, action_index),
            description=safe_description(node), editable=is_editable(node),
            unique_search_input=unique_search,
        )
        if live_kind != kind:
            raise ControlFailure("ACTION_STALE", "surface action semantics changed since the snapshot")
        if (live_risk != "low" or live_effect in ("external_write", "high_risk")) and not strict_frame:
            raise ControlFailure("ACTION_STALE", "sensitive action locator has no exact frame binding")
        if live_risk == "high":
            raise ControlFailure("USER_ACTION_REQUIRED", "high-risk surface action requires direct user interaction")
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
    if return_action_interface:
        return node, scope, validated_interface
    return node, scope


def make_locator(
    path, kind, action_index=None, node=None, surface_generation_value=None,
    screenshot_sha256=None, source=None, bounds_value=None, text=None,
    direction=None, region_signature=None, scope_identity_value=None,
    scope_geometry=None, window_identity_value=None, strict_frame=None,
    rendered_frame_sha256=None, rendered_region_sha256=None,
    accessible_identity_value=None,
):
    version = 4 if surface_generation_value is not None else 2
    value = {"version": version, "path": list(path), "kind": kind}
    if action_index is not None:
        value["action"] = action_index
    if node is not None:
        value["node_signature"] = node_signature(node, path, action_index)
    if surface_generation_value is not None:
        value["surface_generation"] = surface_generation_value
        issued = int(time.time())
        value["issued_unix"] = issued
        value["expires_unix"] = issued + SURFACE_LOCATOR_TTL_SECONDS
    if screenshot_sha256 is not None:
        value["screenshot_sha256"] = screenshot_sha256
    if rendered_frame_sha256 is not None:
        value["rendered_frame_sha256"] = rendered_frame_sha256
    if rendered_region_sha256 is not None:
        value["rendered_region_sha256"] = rendered_region_sha256
    if accessible_identity_value is not None:
        value["accessible_identity"] = accessible_identity_value
    if source is not None:
        value["source"] = source
    if bounds_value is not None:
        value["bounds"] = list(bounds_value)
    if text is not None:
        value["text"] = str(text)
    if direction is not None:
        value["direction"] = direction
    if region_signature is not None:
        value["region_signature"] = region_signature
    if scope_identity_value is not None:
        value["scope_identity"] = scope_identity_value
    if scope_geometry is not None:
        value["scope_geometry"] = list(scope_geometry)
    if window_identity_value is not None:
        value["window_identity"] = window_identity_value
    if version == 4:
        if strict_frame is None:
            strict_frame = False
        if not isinstance(strict_frame, bool):
            raise ControlFailure("CLIENT_INCOMPATIBLE", "strict frame policy must be boolean")
        value["strict_frame"] = strict_frame
    payload = json.dumps(value, separators=(",", ":")).encode("utf-8")
    return base64.urlsafe_b64encode(payload).rstrip(b"=").decode("ascii")


def has_semantic_marker(text, markers):
    text = normalize_cjk_spacing(text).lower()
    for marker in markers:
        lowered = marker.lower()
        if any(ord(character) > 127 for character in lowered):
            if lowered in text:
                return True
            continue
        if re.search(r"(?<![a-z0-9])" + re.escape(lowered) + r"(?![a-z0-9])", text):
            return True
    return False


def classify_action_effect(
    label, role, action_name, unique_search_input=False, description="", editable=False,
):
    text = " ".join((str(label or ""), str(description or ""), str(action_name or ""))).lower()
    if has_semantic_marker(text, DESTRUCTIVE_MARKERS):
        return "activate", "high", "high_risk"
    if has_semantic_marker(text, HIGH_RISK_MARKERS + SECURITY_CHALLENGE_MARKERS):
        return "activate", "high", "high_risk"
    if has_semantic_marker(text, SHARE_MARKERS):
        return "share", "medium", "external_write"
    if editable:
        if unique_search_input and has_semantic_marker(text, SEARCH_MARKERS):
            return "input", "low", "search_input"
        return "input", "medium", "unknown"
    if has_semantic_marker(text, EXTERNAL_WRITE_MARKERS):
        return "activate", "medium", "external_write"
    if has_semantic_marker(text, CLOSE_LABELS):
        return "back", "low", "navigate"
    if has_semantic_marker(text, NAVIGATE_MARKERS):
        return "activate", "low", "navigate"
    return "activate", "medium", "unknown"


def classify_action(label, role, action_name, description="", editable=False):
    kind, risk, _effect = classify_action_effect(
        label, role, action_name, description=description, editable=editable,
    )
    return kind, risk


def locator_frame_fields(frame):
    return {
        "surface_generation_value": frame["generation"],
        "screenshot_sha256": frame["screenshot_sha256"],
        "rendered_frame_sha256": frame["rendered_frame_sha256"],
        "scope_identity_value": scope_identity(frame["scope"]),
        "scope_geometry": main_geometry(frame["scope"]),
        "window_identity_value": frame["window_identity"],
    }


def rectangles_overlap(first, second, threshold=0.45):
    first_x, first_y, first_width, first_height = first
    second_x, second_y, second_width, second_height = second
    overlap_width = max(0, min(first_x + first_width, second_x + second_width) - max(first_x, second_x))
    overlap_height = max(0, min(first_y + first_height, second_y + second_height) - max(first_y, second_y))
    overlap = overlap_width * overlap_height
    minimum = min(first_width * first_height, second_width * second_height)
    return minimum > 0 and overlap / minimum >= threshold


def stable_record_id(prefix, value):
    return prefix + hashlib.sha256(
        json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")
    ).hexdigest()[:24]


def target_id(frame, source, path=None, node=None, region_signature=None):
    if source == "atspi":
        if path is None or node is None:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "semantic target identity is incomplete")
        identity = {"path": list(path), "node": node_replay_identity(node, path)}
    elif source == "ocr":
        if not isinstance(region_signature, str) or not region_signature:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "visual target identity is incomplete")
        identity = {"region": region_signature}
    elif source == "viewport":
        identity = {"viewport": True}
    else:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "unknown semantic target source")
    return stable_record_id("t_", {
        "window_identity": frame["window_identity"],
        "source": source,
        "identity": identity,
    })


def stable_replay_id(frame, target, kind, source, identity, effect=""):
    if effect in ("unknown", "external_write", "high_risk"):
        # Mutation/unknown tombstones must survive layout and X11-window
        # changes. The semantic identity supplied by each source intentionally
        # excludes pixels, bounds, AT-SPI paths and the window identity.
        return stable_record_id("r_", {
            "kind": kind,
            "source": source,
            "identity": identity,
        })
    return stable_record_id("r_", {
        "window_identity": frame["window_identity"],
        "target_id": target,
        "kind": kind,
        "source": source,
        "identity": identity,
    })


def action_context(frame, path):
    root_path = tuple(frame["scope"][1])
    ancestors = []
    for length in range(len(root_path), len(tuple(path))):
        ancestor_path = tuple(path)[:length]
        try:
            ancestor = resolve_path(ancestor_path)
        except ControlFailure:
            continue
        ancestors.append({
            "path": list(ancestor_path), "name": safe_name(ancestor),
            "description": safe_description(ancestor), "role": safe_role(ancestor),
            "bounds": list(bounds(ancestor) or ()),
        })
    topology = []
    for node, node_path in frame["nodes"]:
        interface = action_interface(node)
        if interface is None and not is_editable(node):
            continue
        topology.append({
            "path": list(node_path), "name": safe_name(node),
            "description": safe_description(node), "role": safe_role(node),
            "bounds": list(bounds(node) or ()), "editable": is_editable(node),
            "actions": [
                action_name(interface, index)
                for index in range(min(interface.nActions, 8))
            ] if interface is not None else [],
        })
    return semantic_digest({"ancestors": ancestors, "interactive_topology": topology})


def contextual_action_id(replay_id, context):
    return stable_record_id("a_", {"replay_id": replay_id, "context": context})


def action_instance_context(frame, path, effect, source):
    # Action IDs remain instance capabilities even when replay IDs are logical
    # and cross-window. This prevents a confirmation issued for one rendered
    # window from silently selecting the same-looking action in another.
    context = {
        "semantic": action_context(frame, path),
        "path": list(path),
        "window_identity": frame["window_identity"],
    }
    if effect in ("unknown", "external_write", "high_risk"):
        context["surface_generation"] = frame["generation"]
        context["rendered_frame_sha256"] = frame["rendered_frame_sha256"]
    elif effect in ("navigate", "search_input"):
        context["rendered_frame_sha256"] = frame["rendered_frame_sha256"]
    return context


def public_bounds(frame, geometry):
    if geometry is None:
        return []
    origin_x, origin_y, window_width, window_height = frame["window"]["geometry"]
    x, y, width, height = geometry
    left = max(0, x - origin_x)
    top = max(0, y - origin_y)
    right = min(window_width, x + width - origin_x)
    bottom = min(window_height, y + height - origin_y)
    if right <= left or bottom <= top:
        return []
    return [left, top, right - left, bottom - top]


def atspi_elements_and_assets(frame):
    elements = []
    assets = []
    fields = locator_frame_fields(frame)
    for node, path in frame["nodes"]:
        name = safe_name(node)
        description = safe_description(node)
        role = safe_role(node)
        geometry = bounds(node)
        interface = action_interface(node)
        if geometry is None:
            continue
        if name or description or interface is not None or is_editable(node) or role in IMAGE_ROLES:
            element_target_id = target_id(frame, "atspi", path=path, node=node)
            locator = make_locator(path, "element", node=node, source="atspi", **fields)
            elements.append({
                "id": stable_record_id("e_", [frame["generation"], list(path), node_signature(node, path)]),
                "target_id": element_target_id,
                "label": name,
                "description": description,
                "role": role,
                "bounds": public_bounds(frame, geometry),
                "source": "atspi",
                "confidence": 1.0,
                "editable": is_editable(node),
                "text": text_contents(node)[1] if is_editable(node) and text_contents(node)[0] else None,
                "locator": locator,
            })
        image_hint = role in IMAGE_ROLES or bool(re.search(
            r"(?:image|photo|picture|thumbnail|avatar|图片|图像|头像)",
            (name + " " + description).lower(),
        ))
        if image_hint and len(assets) < MAX_SURFACE_ASSETS:
            locator = make_locator(path, "asset", node=node, source="atspi", **fields)
            token_value = [
                frame["generation"], frame["screenshot_sha256"], list(path),
                node_signature(node, path), list(geometry),
            ]
            token = stable_record_id("asset_", token_value)
            assets.append({
                "id": token,
                "token": token,
                "label": name or description,
                "role": role,
                "bounds": public_bounds(frame, geometry),
                "source": "atspi",
                "confidence": 0.95 if role == "image" else 0.75,
                "locator": locator,
            })
        if len(elements) >= MAX_SURFACE_ELEMENTS:
            break
    return elements, assets


def rendered_viewport_asset(frame):
    width, height = frame["screenshot_dimensions"]
    bounds_value = [0, 0, width, height]
    token = stable_record_id("asset_", [
        frame["generation"], frame["screenshot_sha256"],
        "rendered_viewport", bounds_value,
    ])
    return {
        "id": token,
        "token": token,
        "kind": "rendered_viewport",
        "label": "Rendered viewport",
        "role": "viewport",
        "bounds": bounds_value,
        "source": "rendered",
        "confidence": 1.0,
    }


def semantic_surface_actions(frame):
    actions = []
    seen = set()
    fields = locator_frame_fields(frame)
    search_paths = [
        tuple(path) for node, path in frame["nodes"]
        if is_editable(node) and has_semantic_marker(
            safe_name(node) + " " + safe_description(node), SEARCH_MARKERS,
        )
    ]
    unique_search_path = search_paths[0] if len(search_paths) == 1 else None
    for node, path in frame["nodes"]:
        name = safe_name(node)
        description = safe_description(node)
        label = name or description
        role = safe_role(node)
        geometry = bounds(node)
        interface = action_interface(node)
        if interface is not None:
            for index in range(min(interface.nActions, 8)):
                action_label = action_name(interface, index) or "activate"
                kind, risk, effect = classify_action_effect(
                    name, role, action_label, description=description,
                    editable=is_editable(node),
                    unique_search_input=tuple(path) == unique_search_path,
                )
                locator = make_locator(
                    path, kind, index, node=node, source="atspi",
                    strict_frame=risk != "low", **fields,
                )
                action_target_id = target_id(frame, "atspi", path=path, node=node)
                replay_id = stable_replay_id(
                    frame, action_target_id, kind, "atspi",
                    node_replay_identity(node, path, index), effect,
                )
                action_id = contextual_action_id(
                    replay_id, action_instance_context(frame, path, effect, "atspi"),
                )
                if action_id in seen:
                    continue
                seen.add(action_id)
                actions.append({
                    "id": action_id,
                    "replay_id": replay_id,
                    "target_id": action_target_id,
                    "label": label or action_label,
                    "kind": kind,
                    "risk": risk,
                    "effect": effect,
                    "disabled": risk == "high",
                    "locator": locator,
                    "source": "atspi",
                    "confidence": 1.0,
                    "bounds": public_bounds(frame, geometry),
                })
        elif is_editable(node):
            input_effect = "search_input" if tuple(path) == unique_search_path else "unknown"
            search_input = tuple(path) == unique_search_path
            locator = make_locator(
                path, "input", node=node, source="atspi",
                strict_frame=not search_input, **fields,
            )
            action_target_id = target_id(frame, "atspi", path=path, node=node)
            replay_id = stable_replay_id(
                frame, action_target_id, "input", "atspi", node_replay_identity(node, path),
                input_effect,
            )
            action_id = contextual_action_id(
                replay_id, action_instance_context(frame, path, input_effect, "atspi"),
            )
            if action_id not in seen:
                seen.add(action_id)
                actions.append({
                    "id": action_id,
                    "replay_id": replay_id,
                    "target_id": action_target_id,
                    "label": label or "Text input",
                    "kind": "input",
                    "risk": "low" if search_input else "medium",
                    "effect": input_effect,
                    "disabled": False,
                    "locator": locator,
                    "source": "atspi",
                    "confidence": 1.0,
                    "bounds": public_bounds(frame, geometry),
                })
        if len(actions) >= MAX_SURFACE_ACTIONS - 2:
            break
    return actions


def ocr_elements_and_actions(frame, atspi_elements, atspi_actions):
    elements = []
    actions = []
    root, root_path = frame["scope"]
    fields = locator_frame_fields(frame)
    atspi_regions = [
        (normalized_exact(item["label"] or item["description"]), tuple(item["bounds"]), item["target_id"])
        for item in atspi_elements
        if (item["label"] or item["description"]) and len(item["bounds"]) == 4
    ]
    actionable_targets = {
        item.get("target_id") for item in atspi_actions if item.get("target_id")
    }
    for region in frame["ocr"]:
        normalized = normalized_exact(region["text"])
        if not normalized:
            continue
        public_region = public_bounds(frame, region["bounds"])
        matching_atspi = [
            target for label, geometry, target in atspi_regions
            if normalized == label and rectangles_overlap(public_region, geometry)
        ]
        signature = ocr_region_signature(frame, region)
        visual_target_id = (
            matching_atspi[0] if len(set(matching_atspi)) == 1
            else target_id(frame, "ocr", region_signature=signature)
        )
        locator = make_locator(
            root_path, "element", node=root, source="ocr",
            bounds_value=region["bounds"], text=region["text"],
            region_signature=signature, **fields,
        )
        if not matching_atspi:
            elements.append({
                "id": stable_record_id("e_", [frame["generation"], signature]),
                "target_id": visual_target_id,
                "label": region["text"],
                "description": "",
                "role": "ocr text",
                "bounds": public_region,
                "source": "ocr",
                "confidence": round(region["confidence"], 3),
                "editable": False,
                "text": region["text"],
                "locator": locator,
            })
        kind, risk, effect = classify_ocr_action_effect(region["text"])
        action_locator = make_locator(
            root_path, kind, node=root, source="ocr",
            bounds_value=region["bounds"], text=region["text"],
            region_signature=signature, strict_frame=True, **fields,
        )
        if not any(target in actionable_targets for target in matching_atspi):
            # Pixel signatures bind the one-shot locator and target to this exact
            # rendering. Replay identity stays semantic so a changed hover color
            # cannot resurrect a confirmed write under a fresh action ID.
            replay_id = stable_replay_id(
                frame, visual_target_id, kind, "ocr", {
                "kind": kind,
                "text": normalized,
            }, effect)
            actions.append({
                "id": contextual_action_id(
                    replay_id, action_instance_context(frame, root_path, effect, "ocr"),
                ),
                "replay_id": replay_id,
                "target_id": visual_target_id,
                "label": region["text"],
                "kind": kind,
                "risk": risk,
                "effect": effect,
                "disabled": risk == "high",
                "locator": action_locator,
                "source": "ocr",
                "confidence": round(region["confidence"], 3),
                "bounds": public_region,
            })
        if len(elements) >= MAX_SURFACE_ELEMENTS or len(actions) >= MAX_SURFACE_ACTIONS - 2:
            break
    return elements, actions


def high_risk_visual_regions(frame):
    return [
        region for region in frame["ocr"]
        if classify_action_effect(
            region["text"], "ocr text", "visual activate",
        )[1] == "high"
    ]


def fuse_cross_source_high_risk(frame, actions):
    visual = [
        public_bounds(frame, region["bounds"])
        for region in high_risk_visual_regions(frame)
    ]
    visual = [geometry for geometry in visual if valid_rectangle(geometry)]
    if not visual:
        return
    for action in actions:
        if action.get("source") != "atspi" or not valid_rectangle(action.get("bounds")):
            continue
        if any(
            rectangles_overlap(action["bounds"], geometry, threshold=0.6)
            for geometry in visual
        ):
            action["risk"] = "high"
            action["effect"] = "high_risk"
            action["disabled"] = True


def viewport_scroll_actions(frame):
    root, root_path = frame["scope"]
    fields = locator_frame_fields(frame)
    viewport_target_id = target_id(frame, "viewport")
    actions = []
    for direction in ("up", "down"):
        locator = make_locator(
            root_path, "scroll", node=root, source="viewport", direction=direction,
            **fields,
        )
        replay_id = stable_replay_id(
            frame, viewport_target_id, "scroll", "viewport", direction,
        )
        actions.append({
            "id": contextual_action_id(replay_id, frame["rendered_frame_sha256"]),
            "replay_id": replay_id,
            "target_id": viewport_target_id,
            "label": "Scroll viewport " + direction,
            "kind": "scroll",
            "risk": "low",
            "effect": "observe",
            "disabled": False,
            "locator": locator,
            "source": "viewport",
            "confidence": 1.0,
        })
    return actions


def miniprogram_surface(frame):
    return frame["window"].get("process_kind") == "miniprogram"


def surface_snapshot(frame=None, expected_kind=None, expected_window_identity=None):
    frame = frame or capture_surface_frame(expected_window_identity)
    require_safe_surface_frame(frame)
    process_kind = frame["window"].get("process_kind")
    if process_kind not in ("miniprogram", "web"):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "the main WeChat window is not a generic surface")
    if expected_kind is not None and expected_kind != process_kind:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "opened window is not a verified " + expected_kind + " surface")
    root, _root_path = frame["scope"]
    strings = collect_strings(frame["nodes"], limit=300)
    seen_text = {normalized_exact(value) for value in strings}
    for region in frame["ocr"]:
        key = normalized_exact(region["text"])
        if key and key not in seen_text:
            strings.append(region["text"])
            seen_text.add(key)
    semantic = "\n".join(strings)[:MAX_SEMANTIC_TEXT]
    title = safe_name(root)
    urls = re.findall(r"https?://[^\s<>\"]+", semantic)
    elements, assets = atspi_elements_and_assets(frame)
    # Canvas-only mini programs may expose no accessibility image node at all.
    # A generation-bound full-viewport asset guarantees a controlled rendered
    # export without inventing arbitrary crop coordinates or claiming access to
    # an original media file. Reserve one slot even when many semantic images
    # are present.
    assets = assets[:max(0, MAX_SURFACE_ASSETS - 1)]
    assets.append(rendered_viewport_asset(frame))
    semantic_actions = semantic_surface_actions(frame)
    ocr_elements, ocr_actions = ocr_elements_and_actions(frame, elements, semantic_actions)
    elements = (elements + ocr_elements)[:MAX_SURFACE_ELEMENTS]
    actions = (semantic_actions + ocr_actions)[:MAX_SURFACE_ACTIONS - 2]
    fuse_cross_source_high_risk(frame, actions)
    actions.extend(viewport_scroll_actions(frame))
    if high_risk_surface_context(frame):
        for action in actions:
            if high_risk_context_allows(action["kind"], action["label"]):
                continue
            action["risk"] = "high"
            action["effect"] = "high_risk"
            action["disabled"] = True
    kind = process_kind
    viewport = {
        "bounds": [0, 0, frame["window"]["geometry"][2], frame["window"]["geometry"][3]],
        "generation": frame["generation"],
        "screenshot_sha256": frame["screenshot_sha256"],
        "window_identity": frame["window_identity"],
        "directions": ["up", "down"],
    }
    return {
        "kind": kind,
        "title": title,
        "url": urls[0] if urls else "",
        "semantic_text": semantic,
        "generation": frame["generation"],
        "screenshot_base64": base64.b64encode(frame["screenshot"]).decode("ascii"),
        "screenshot_sha256": frame["screenshot_sha256"],
        "window_identity": frame["window_identity"],
        "elements": elements,
        "assets": assets,
        "viewport": viewport,
        "actions": actions,
    }


def scope_identity(scope):
    root, root_path = scope
    return semantic_digest({
        "path": list(root_path),
        "role": safe_role(root),
        "bounds": list(bounds(root) or ()),
    })


def require_same_window(before, after):
    if scope_identity(before) != scope_identity(after):
        raise ControlFailure("ACTION_STALE", "active WeChat window changed during the operation")


def unique_auth_editor(scope):
    nodes = scope_nodes(scope)
    if not authentication_evidence(nodes)["sms"]:
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
    if verified_window_identity(scope).get("process_kind") != "wechat":
        raise ControlFailure(
            "CLIENT_INCOMPATIBLE",
            "authentication codes can only be entered in the official main WeChat window",
        )
    editor, editor_path = unique_auth_editor(scope)
    editor_role = safe_role(editor)
    editor_bounds = bounds(editor)
    # Resolve every semantic target before mutating the code field.
    unique_named_node(CONFIRM_LABELS, scope, purpose="authentication confirmation button")
    value = str(code)
    set_text(editor, value, scope, editor_path)
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
        node, surface_scope, validated_interface = validate_surface_locator(
            locator, expected_kind="share", require_unique_kind=True,
            return_action_interface=True,
        )
        activate_locator(
            node, locator, "", surface_scope, validated_interface=validated_interface,
        )
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
        set_text(editor, text, scope, editor_path)
        verify_text_contents(editor, text)
    else:
        # Never let a pre-existing draft hitch a ride on an attachment-only send.
        verify_text_contents(editor, "")
    if validated_attachments:
        stage_clipboard_files(validated_attachments, editor, editor_path, scope)
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


def unique_main_search_editor(frame):
    if frame["window"].get("process_kind") != "wechat":
        raise ControlFailure("CLIENT_INCOMPATIBLE", "named mini-program search must start in the official WeChat main window")
    root_x, root_y, root_width, root_height = main_geometry(frame["scope"])
    candidates = []
    for node, path in frame["nodes"]:
        label = (safe_name(node) + " " + safe_description(node)).lower()
        geometry = bounds(node)
        if not is_editable(node) or geometry is None:
            continue
        x, y, width, height = geometry
        if not any(marker in label for marker in SEARCH_MARKERS):
            continue
        if y + height / 2 > root_y + root_height * 0.35:
            continue
        if x + width / 2 > root_x + root_width * 0.55:
            continue
        candidates.append((node, path))
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple upper-left WeChat search fields are visible")
    if not candidates:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "the unique upper-left WeChat search field is not accessible")
    return candidates[0]


def common_path(first, second):
    result = []
    for left, right in zip(first, second):
        if left != right:
            break
        result.append(left)
    return tuple(result)


def miniprogram_context(geometry, path, frame):
    root_path = tuple(frame["scope"][1])
    marker_values = {normalized_exact(value) for value in MINIPROGRAM_MARKERS}
    if path is not None:
        for node, marker_path in frame["nodes"]:
            marker_texts = (safe_name(node), safe_description(node))
            if not any(normalized_exact(value) in marker_values for value in marker_texts if value):
                continue
            marker_path = tuple(marker_path)
            marker_geometry = bounds(node)
            if marker_geometry is None or marker_path == root_path:
                continue
            if path_is_within(path, marker_path) and tuple(path) != marker_path:
                proof = {
                    "kind": "atspi-ancestor", "marker": list(marker_path),
                    "marker_signature": node_signature(node, marker_path),
                }
                return semantic_digest(proof)
            section_path = marker_path[:-1]
            if (
                len(section_path) <= len(root_path)
                or not path_is_within(path, section_path)
                or common_path(path, marker_path) != section_path
            ):
                continue
            try:
                section_geometry = bounds(resolve_path(section_path))
            except ControlFailure:
                continue
            if (
                section_geometry is not None
                and geometry_contains(section_geometry, geometry)
                and geometry_contains(section_geometry, marker_geometry)
            ):
                proof = {
                    "kind": "atspi-section", "section": list(section_path),
                    "marker": list(marker_path),
                    "section_signature": node_signature(resolve_path(section_path), section_path),
                    "marker_signature": node_signature(node, marker_path),
                }
                return semantic_digest(proof)
        return None

    x, y, width, height = geometry
    root_height = main_geometry(frame["scope"])[3]
    conflicts = [
        item for item in frame["ocr"]
        if any(marker in normalized_exact(item["text"]) for marker in NON_MINIPROGRAM_SECTION_MARKERS)
    ]
    for item in frame["ocr"]:
        if normalized_exact(item["text"]) not in marker_values:
            continue
        marker_x, marker_y, marker_width, marker_height = item["bounds"]
        marker_bottom = marker_y + marker_height
        vertical_gap = y - marker_bottom
        horizontally_aligned = (
            x + width >= marker_x - 24
            and x <= marker_x + max(marker_width + 320, width * 2)
        )
        if vertical_gap < 0 or vertical_gap > min(160, root_height * 0.22) or not horizontally_aligned:
            continue
        if any(
            marker_bottom <= conflict["bounds"][1] <= y
            for conflict in conflicts
        ):
            continue
        return semantic_digest({
            "kind": "ocr-section", "marker_text": normalized_exact(item["text"]),
            "marker_bounds": list(item["bounds"]), "target_bounds": list(geometry),
        })
    return None


def actionable_ancestor(path, root_path):
    for length in range(len(path), len(root_path), -1):
        candidate_path = tuple(path[:length])
        node = resolve_path(candidate_path)
        action_index = named_launch_action_index(node)
        if action_index is not None:
            return node, candidate_path, action_index
    return None


def named_miniprogram_candidates(name, frame):
    normalized = normalized_exact(name)
    exact_seen = False
    candidates = []
    root_path = frame["scope"][1]
    for node, path in frame["nodes"]:
        node_label = safe_name(node) or safe_description(node)
        if normalized_exact(node_label) != normalized:
            continue
        geometry = bounds(node)
        if geometry is None:
            continue
        exact_seen = True
        section_identity = miniprogram_context(geometry, path, frame)
        if section_identity is None:
            continue
        target = actionable_ancestor(path, root_path)
        if target is None:
            continue
        target_node, target_path, action_index = target
        candidates.append({
            "source": "atspi",
            "path": tuple(target_path),
            "action": action_index,
            "bounds": tuple(geometry),
            "signature": node_signature(target_node, target_path, action_index),
            "name_path": tuple(path),
            "name_signature": node_signature(node, path),
            "section_identity": section_identity,
        })
    for region in frame["ocr"]:
        if normalized_exact(region["text"]) != normalized:
            continue
        exact_seen = True
        section_identity = miniprogram_context(region["bounds"], None, frame)
        if section_identity is None:
            continue
        if any(rectangles_overlap(region["bounds"], item["bounds"], threshold=0.6) for item in candidates):
            continue
        candidates.append({
            "source": "ocr",
            "path": tuple(root_path),
            "action": None,
            "bounds": tuple(region["bounds"]),
            "text": region["text"],
            "signature": ocr_region_signature(frame, region),
            "name_path": tuple(root_path),
            "name_signature": ocr_region_signature(frame, region),
            "section_identity": section_identity,
        })
    deduplicated = []
    seen = set()
    for candidate in candidates:
        key = (
            (candidate["source"], candidate["path"], candidate["action"])
            if candidate["source"] == "atspi"
            else (candidate["source"], candidate["bounds"])
        )
        if key not in seen:
            seen.add(key)
            deduplicated.append(candidate)
    return deduplicated, exact_seen


def candidate_identity(candidate):
    return semantic_digest({
        "source": candidate["source"],
        "path": list(candidate["path"]),
        "action": candidate["action"],
        "bounds": list(candidate["bounds"]),
        "signature": candidate["signature"],
        "name_path": list(candidate["name_path"]),
        "name_signature": candidate["name_signature"],
        "section_identity": candidate["section_identity"],
    })


def unique_named_miniprogram_candidate(name, frame):
    candidates, exact_seen = named_miniprogram_candidates(name, frame)
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple exact mini-program results have the requested name")
    if not candidates:
        if exact_seen:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "the exact result is not proven to be in a mini-program section")
        raise ControlFailure("SURFACE_MISSING", "no exact mini-program result is visible")
    return candidates[0]


def activate_named_candidate(candidate, frame):
    if candidate["source"] == "ocr":
        region = {"text": candidate["text"], "bounds": candidate["bounds"]}
        if ocr_region_signature(frame, region) != candidate["signature"]:
            raise ControlFailure("ACTION_STALE", "visual mini-program result changed before activation")
        verified = revalidate_visual_region(
            frame["window_identity"], region, candidate["signature"],
            frame["rendered_frame_sha256"], frame["generation"],
        )
        visual_pointer_action(
            candidate["bounds"], 1,
            expected_window_identity=frame["window_identity"],
            expected_rendered_frame_sha256=verified["rendered_frame_sha256"],
        )
        return
    name_node = resolve_path(candidate["name_path"])
    if node_signature(name_node, candidate["name_path"]) != candidate["name_signature"]:
        raise ControlFailure("ACTION_STALE", "mini-program result name changed before activation")
    if miniprogram_context(candidate["bounds"], candidate["name_path"], frame) != candidate["section_identity"]:
        raise ControlFailure("ACTION_STALE", "mini-program result section changed before activation")
    node = resolve_path(candidate["path"])
    interface = action_interface(node)
    if interface is None:
        raise ControlFailure("ACTION_STALE", "mini-program result action is unavailable")
    if node_signature(
        node, candidate["path"], candidate["action"], interface=interface,
    ) != candidate["signature"]:
        raise ControlFailure("ACTION_STALE", "mini-program result changed before activation")
    if named_launch_action_index_from_interface(node, interface) != candidate["action"]:
        raise ControlFailure("ACTION_STALE", "mini-program result action changed before activation")
    if not interface.doAction(candidate["action"]):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "mini-program result rejected activation")


def wait_for_named_candidate(name, expected_window_identity):
    deadline = time.monotonic() + NAMED_SEARCH_TIMEOUT_SECONDS
    previous_identity = None
    last_error = None
    while time.monotonic() < deadline:
        frame = capture_surface_frame(expected_window_identity)
        require_safe_surface_frame(frame)
        try:
            candidate = unique_named_miniprogram_candidate(name, frame)
        except ControlFailure as exc:
            if exc.code == "TARGET_AMBIGUOUS":
                raise
            if exc.code not in ("SURFACE_MISSING", "CLIENT_INCOMPATIBLE"):
                raise
            last_error = exc
            time.sleep(NAMED_POLL_SECONDS)
            continue
        identity = candidate_identity(candidate)
        if identity == previous_identity:
            return frame, candidate
        previous_identity = identity
        time.sleep(NAMED_POLL_SECONDS)
    if last_error is not None:
        raise last_error
    raise ControlFailure("ACTION_STALE", "mini-program search result did not remain stable")


def miniprogram_name_visible(name, frame):
    expected = normalized_exact(name)
    root_x, root_y, root_width, root_height = main_geometry(frame["scope"])
    for node, _path in frame["nodes"]:
        geometry = bounds(node)
        if geometry is None or geometry[1] > root_y + root_height * 0.3:
            continue
        if expected in (normalized_exact(safe_name(node)), normalized_exact(safe_description(node))):
            return True
    for item in frame["ocr"]:
        if item["bounds"][1] <= root_y + root_height * 0.3 and normalized_exact(item["text"]) == expected:
            return True
    return False


def window_instance(identity):
    return (
        identity["xid"], identity["pid"], identity["pid_starttime"],
        identity["process_kind"],
    )


def related_window_identities(first, second):
    first_lineage = {
        (item.get("pid"), item.get("starttime"))
        for item in first.get("lineage", []) if isinstance(item, dict)
    }
    second_lineage = {
        (item.get("pid"), item.get("starttime"))
        for item in second.get("lineage", []) if isinstance(item, dict)
    }
    return bool(first_lineage and second_lineage and first_lineage.intersection(second_lineage))


def wait_for_opened_miniprogram(name, initial_window_identity, previous_windows):
    initial_identity = decode_window_identity(initial_window_identity)
    deadline = time.monotonic() + NAMED_OPEN_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        frame = capture_surface_frame()
        require_safe_surface_frame(frame)
        if frame["window_identity"] == initial_window_identity:
            time.sleep(NAMED_POLL_SECONDS)
            continue
        if frame["window"].get("process_kind") != "miniprogram":
            raise ControlFailure("CLIENT_INCOMPATIBLE", "named result opened an unverified non-mini-program window")
        if window_instance(frame["window"]) in previous_windows:
            raise ControlFailure("ACTION_STALE", "named result focused a pre-existing mini-program window")
        if not related_window_identities(initial_identity, frame["window"]):
            raise ControlFailure("ACTION_STALE", "named result opened outside the bound WeChat process family")
        if not miniprogram_name_visible(name, frame):
            time.sleep(NAMED_POLL_SECONDS)
            continue
        return frame
    raise ControlFailure("SURFACE_MISSING", "verified mini-program window did not appear before the deadline")


def wait_for_new_surface(initial_window_identity, previous_windows, expected_kind):
    initial_identity = decode_window_identity(initial_window_identity)
    deadline = time.monotonic() + NAMED_OPEN_TIMEOUT_SECONDS
    while time.monotonic() < deadline:
        frame = capture_surface_frame()
        require_safe_surface_frame(frame)
        if frame["window_identity"] == initial_window_identity:
            time.sleep(NAMED_POLL_SECONDS)
            continue
        if frame["window"].get("process_kind") != expected_kind:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "message card opened an unexpected surface kind")
        if window_instance(frame["window"]) in previous_windows:
            raise ControlFailure("ACTION_STALE", "message card focused a pre-existing surface window")
        if not related_window_identities(initial_identity, frame["window"]):
            raise ControlFailure("ACTION_STALE", "opened surface is outside the bound WeChat process family")
        return frame
    raise ControlFailure("SURFACE_MISSING", "message card did not open a verified surface before the deadline")


def open_named_surface(request):
    kind = str(request.get("kind") or "").strip().lower()
    if kind != "miniprogram":
        raise ControlFailure("CLIENT_INCOMPATIBLE", "open_named_surface requires kind=miniprogram and an exact name")
    name = validated_surface_name(request.get("name"))

    initial = main_wechat_frame_for_navigation()
    if initial["window"].get("process_kind") != "wechat":
        raise ControlFailure("CLIENT_INCOMPATIBLE", "named mini-program launch requires the main WeChat window")
    editor, editor_path = unique_main_search_editor(initial)
    editor_signature = node_signature(editor, editor_path)
    initial_window = initial["window_identity"]

    # Re-observe the exact same frame before mutating the global search field.
    confirmed = capture_surface_frame(initial_window)
    if confirmed["generation"] != initial["generation"]:
        raise ControlFailure("ACTION_STALE", "WeChat main window changed before search input")
    current_editor, current_path = unique_main_search_editor(confirmed)
    if tuple(current_path) != tuple(editor_path) or node_signature(current_editor, current_path) != editor_signature:
        raise ControlFailure("ACTION_STALE", "WeChat search field changed before input")
    set_text(current_editor, name, confirmed["scope"], current_path)
    verify_text_contents(current_editor, name)
    confirmed_results, confirmed_candidate = wait_for_named_candidate(name, initial_window)
    expected_candidate = candidate_identity(confirmed_candidate)
    previous_windows = x11_wechat_window_inventory()
    final_results = capture_surface_frame(initial_window)
    require_safe_surface_frame(final_results)
    final_candidate = unique_named_miniprogram_candidate(name, final_results)
    if candidate_identity(final_candidate) != expected_candidate:
        raise ControlFailure("ACTION_STALE", "mini-program result changed before activation")
    activate_named_candidate(final_candidate, final_results)
    opened = wait_for_opened_miniprogram(name, initial_window, previous_windows)
    return surface_snapshot(opened, expected_kind="miniprogram")


def open_surface(request):
    title = str(request.get("conversation_title") or "").strip()
    conversation_locator = str(request.get("conversation_locator") or "")
    label = str(request.get("accessible_label") or "").strip()
    expected_kind = str(request.get("kind") or "").strip().lower()
    surface_locator = str(request.get("surface_locator") or "")
    if (
        not title or not conversation_locator or not label or not surface_locator
        or expected_kind not in ("web", "miniprogram")
    ):
        raise ControlFailure(
            "CLIENT_INCOMPATIBLE",
            "surface needs an exact conversation, semantic label, and signed message locator",
        )
    main_wechat_frame_for_navigation()
    scope = select_conversation(title, conversation_locator)
    selected_window = verified_window_identity(scope)
    if selected_window.get("process_kind") != "wechat":
        raise ControlFailure("ACTION_STALE", "message-backed surface did not start in the main WeChat window")
    selected_window_identity = encode_window_identity(selected_window)
    initial = capture_surface_frame(selected_window_identity)
    require_safe_surface_frame(initial)
    if initial["window"].get("process_kind") != "wechat":
        raise ControlFailure("ACTION_STALE", "message-backed surface did not start in the main WeChat window")
    require_same_window(scope, initial["scope"])
    verify_conversation_header(title, initial["scope"])
    candidate = validate_message_surface_candidate(
        label, expected_kind, surface_locator, initial,
    )
    previous_windows = x11_wechat_window_inventory()
    confirmed = capture_surface_frame(initial["window_identity"])
    require_safe_surface_frame(confirmed)
    require_same_window(initial["scope"], confirmed["scope"])
    verify_conversation_header(title, confirmed["scope"])
    confirmed_candidate = validate_message_surface_candidate(
        label, expected_kind, surface_locator, confirmed,
    )
    if confirmed_candidate != candidate:
        raise ControlFailure("ACTION_STALE", "message card changed before activation")
    activate_exact_action_candidate(confirmed_candidate)
    opened = wait_for_new_surface(initial["window_identity"], previous_windows, expected_kind)
    return surface_snapshot(opened, expected_kind=expected_kind)


def x11_record_value(record, name):
    try:
        return getattr(record, name)
    except (AttributeError, KeyError):
        try:
            return record[name]
        except (KeyError, TypeError):
            raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer metadata is incomplete")


def x11_bound_window(connection, expected_identity, x_module):
    root = connection.screen().root
    active_atom = connection.intern_atom("_NET_ACTIVE_WINDOW", only_if_exists=True)
    if not active_atom:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 active-window identity is unavailable")
    active_property = root.get_full_property(active_atom, x_module.AnyPropertyType)
    try:
        active_xid = int(active_property.value[0])
    except (AttributeError, IndexError, TypeError, ValueError) as exc:
        raise ControlFailure("ACTION_STALE", "active X11 window changed before pointer injection") from exc
    expected_xid = expected_identity.get("xid")
    if (
        not isinstance(expected_xid, str)
        or not re.fullmatch(r"0x[0-9a-f]+", expected_xid)
        or active_xid != int(expected_xid, 16)
    ):
        raise ControlFailure("ACTION_STALE", "active X11 window changed before pointer injection")
    window = connection.create_resource_object("window", active_xid)
    window_geometry = window.get_geometry()
    translated = window.translate_coords(root, 0, 0)
    geometry = (
        int(translated.x), int(translated.y),
        int(window_geometry.width), int(window_geometry.height),
    )
    if list(geometry) != expected_identity.get("geometry"):
        raise ControlFailure("ACTION_STALE", "bound X11 window geometry changed before pointer injection")
    pid_atom = connection.intern_atom("_NET_WM_PID", only_if_exists=True)
    pid_property = window.get_full_property(pid_atom, x_module.AnyPropertyType) if pid_atom else None
    try:
        pid = int(pid_property.value[0])
    except (AttributeError, IndexError, TypeError, ValueError) as exc:
        raise ControlFailure("ACTION_STALE", "bound X11 window owner changed before pointer injection") from exc
    if pid != expected_identity.get("pid"):
        raise ControlFailure("ACTION_STALE", "bound X11 window owner changed before pointer injection")
    process = process_record(pid)
    if process["starttime"] != expected_identity.get("pid_starttime"):
        raise ControlFailure("ACTION_STALE", "bound X11 process changed before pointer injection")
    return root, window, geometry


def x11_window_rgba(connection, window, geometry, x_module):
    width, height = geometry[2:]
    if width <= 0 or height <= 0 or width * height > MAX_SCREENSHOT_PIXELS:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer dimensions are outside the safe limit")
    attributes = window.get_attributes()
    image = window.get_image(0, 0, width, height, x_module.ZPixmap, 0xFFFFFFFF)
    if image is None:
        raise ControlFailure("ACTION_STALE", "bound X11 window pixels are unavailable")
    visual_id = int(x11_record_value(attributes, "visual"))
    depth = int(x11_record_value(image, "depth"))
    visuals = [
        visual
        for allowed_depth in connection.screen().allowed_depths
        if int(x11_record_value(allowed_depth, "depth")) == depth
        for visual in x11_record_value(allowed_depth, "visuals")
        if int(x11_record_value(visual, "visual_id")) == visual_id
    ]
    if len(visuals) != 1 or int(x11_record_value(visuals[0], "visual_class")) != x_module.TrueColor:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "bound X11 window does not use one verifiable TrueColor visual")
    formats = [
        item for item in connection.display.info.pixmap_formats
        if int(x11_record_value(item, "depth")) == depth
    ]
    if len(formats) != 1:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer format is ambiguous")
    bits_per_pixel = int(x11_record_value(formats[0], "bits_per_pixel"))
    scanline_pad = int(x11_record_value(formats[0], "scanline_pad"))
    if bits_per_pixel != 32 or scanline_pad != 32:
        # The runtime owns a 24-bit Xvfb whose ZPixmap transport is 32 bpp.
        # Refuse an untested packing instead of hashing the wrong colors.
        raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer packing is unsupported")
    data = bytes(x11_record_value(image, "data"))
    if len(data) != width * height * 4:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer byte length is inconsistent")
    byte_order = int(connection.display.info.image_byte_order)
    if byte_order not in (x_module.LSBFirst, x_module.MSBFirst):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer byte order is unsupported")

    visual = visuals[0]
    offsets = []
    for channel in ("red_mask", "green_mask", "blue_mask"):
        mask = int(x11_record_value(visual, channel))
        if mask <= 0 or mask & (mask + (mask & -mask)):
            raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer color mask is unsupported")
        shift = (mask & -mask).bit_length() - 1
        if mask != 0xFF << shift or shift % 8:
            raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer color precision is unsupported")
        byte_offset = shift // 8
        if byte_order == x_module.MSBFirst:
            byte_offset = 3 - byte_offset
        offsets.append(byte_offset)
    if len(set(offsets)) != 3 or any(offset < 0 or offset > 3 for offset in offsets):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "X11 framebuffer color layout is invalid")
    rgba = bytearray(width * height * 4)
    rgba[0::4] = data[offsets[0]::4]
    rgba[1::4] = data[offsets[1]::4]
    rgba[2::4] = data[offsets[2]::4]
    rgba[3::4] = b"\xff" * (width * height)
    return bytes(rgba), (width, height)


def visual_pointer_action(
    geometry, button, repetitions=1, expected_window_identity=None,
    expected_rendered_frame_sha256=None,
):
    if geometry is None:
        raise ControlFailure("ACTION_STALE", "visual target has no usable geometry")
    x, y, width, height = geometry
    if width <= 0 or height <= 0:
        raise ControlFailure("ACTION_STALE", "visual target has no usable geometry")
    expected_identity = decode_window_identity(expected_window_identity)
    expected_geometry_value = expected_identity.get("geometry")
    if not valid_rectangle(expected_geometry_value):
        raise ControlFailure("ACTION_STALE", "bound X11 window geometry is invalid")
    expected_window_geometry = tuple(expected_geometry_value)
    if not fully_contained_geometry(
        expected_window_geometry, tuple(geometry),
    ):
        raise ControlFailure("ACTION_STALE", "visual target is outside the bound X11 window")
    if expected_rendered_frame_sha256 is not None and (
        not isinstance(expected_rendered_frame_sha256, str)
        or not re.fullmatch(r"[0-9a-f]{64}", expected_rendered_frame_sha256)
    ):
        raise ControlFailure("ACTION_STALE", "visual action has no complete rendered-frame binding")
    center_x = x + width // 2
    center_y = y + height // 2
    from Xlib import X, display
    from Xlib.ext import xtest
    connection = display.Display()
    grabbed = False
    try:
        # XGrabServer makes the exact-window/full-frame check and XTest enqueue
        # one X11 critical section. No other client can focus, resize or redraw
        # the target between the digest comparison and pointer injection.
        connection.grab_server()
        grabbed = True
        root, window, current_geometry = x11_bound_window(connection, expected_identity, X)
        if expected_rendered_frame_sha256 is not None:
            pixels, dimensions = x11_window_rgba(connection, window, current_geometry, X)
            if rendered_frame_digest(pixels, dimensions) != expected_rendered_frame_sha256:
                raise ControlFailure("ACTION_STALE", "complete rendered surface changed before pointer injection")
        xtest.fake_input(
            connection, X.MotionNotify, x=center_x, y=center_y, root=root,
        )
        for _index in range(max(1, min(int(repetitions), 6))):
            xtest.fake_input(connection, X.ButtonPress, button)
            xtest.fake_input(connection, X.ButtonRelease, button)
        connection.sync()
    finally:
        if grabbed:
            connection.ungrab_server()
            connection.sync()
        connection.close()


def unique_focused_visual_editor(scope, target_geometry):
    focused_state = getattr(pyatspi, "STATE_FOCUSED", None)
    if focused_state is None:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "focused accessibility state is unavailable")
    candidates = []
    for candidate, path in visible_scope_nodes(scope):
        geometry = bounds(candidate)
        if (
            geometry is not None and is_editable(candidate)
            and safe_state(candidate, focused_state)
            and rectangles_overlap(geometry, target_geometry, threshold=0.1)
        ):
            candidates.append((candidate, path))
    if len(candidates) > 1:
        raise ControlFailure("TARGET_AMBIGUOUS", "multiple focused editors overlap the visual input target")
    if not candidates:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "visual input did not focus a verifiable editor")
    return candidates[0]


def activate_locator(node, locator, text, scope=None, validated_interface=None):
    kind = locator.get("kind")
    source = locator.get("source", "atspi")
    if kind == "scroll":
        direction = locator.get("direction")
        visual_pointer_action(
            bounds(node), 4 if direction == "up" else 5, repetitions=4,
            expected_window_identity=locator.get("window_identity"),
        )
        return
    if source == "ocr":
        geometry = tuple(locator["bounds"])
        region = {"text": locator["text"], "bounds": geometry}
        live_kind, live_risk, _live_effect = classify_ocr_action_effect(region["text"])
        if live_kind != kind:
            raise ControlFailure("ACTION_STALE", "visual target action semantics changed")
        if live_risk == "high":
            raise ControlFailure("USER_ACTION_REQUIRED", "high-risk visual action requires direct user interaction")
        verified = revalidate_visual_region(
            locator.get("window_identity"), region, locator["region_signature"],
            locator.get("rendered_frame_sha256"), locator.get("surface_generation"),
            action_kind=kind,
        )
        current_scope = verified["scope"]
        visual_pointer_action(
            geometry, 1,
            expected_window_identity=locator.get("window_identity"),
            expected_rendered_frame_sha256=verified["rendered_frame_sha256"],
        )
        if kind == "input":
            time.sleep(0.1)
            current_scope = active_surface_root()
            if scope is not None:
                require_same_window(scope, current_scope)
            require_window_identity(
                locator.get("window_identity"), verified_window_identity(current_scope),
            )
            editor, editor_path = unique_focused_visual_editor(current_scope, geometry)
            set_text(editor, str(text or ""), current_scope, editor_path)
            verify_text_contents(editor, str(text or ""))
        elif kind != "visual_activate":
            raise ControlFailure("CLIENT_INCOMPATIBLE", "visual locator has an unsupported action kind")
        return
    if kind == "input":
        set_text(node, str(text or ""), scope, locator.get("path"))
        verify_text_contents(node, str(text or ""))
        return
    interface = validated_interface if validated_interface is not None else action_interface(node)
    index = locator.get("action")
    if interface is None or not isinstance(index, int) or index < 0 or index >= interface.nActions:
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface action is no longer available")
    validate_node_signature(node, locator, interface=interface)
    _kind, risk, _effect = classify_action_effect(
        safe_name(node), safe_role(node), action_name(interface, index),
        description=safe_description(node), editable=is_editable(node),
    )
    if risk == "high":
        raise ControlFailure("USER_ACTION_REQUIRED", "high-risk surface action requires direct user interaction")
    if not interface.doAction(index):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "surface action was rejected")


def act_surface(request):
    locator = decode_locator(request.get("locator"))
    expected_window = request.get("expected_window_identity")
    if not isinstance(expected_window, str) or expected_window != locator.get("window_identity"):
        raise ControlFailure("ACTION_STALE", "surface action is not bound to the expected window")
    previous_windows = x11_wechat_window_inventory()
    node, scope, validated_interface = validate_surface_locator(
        locator, require_unique_kind=locator.get("kind") == "share",
        return_action_interface=True,
    )
    requested_text = str(request.get("text") or "")
    activate_locator(
        node, locator, requested_text, scope, validated_interface=validated_interface,
    )
    time.sleep(0.4)
    same_window = locator.get("kind") in ("input", "scroll")
    frame = capture_surface_frame(
        locator.get("window_identity") if same_window else None,
    )
    if frame["window_identity"] != locator.get("window_identity"):
        initial_identity = decode_window_identity(locator["window_identity"])
        if window_instance(frame["window"]) in previous_windows:
            raise ControlFailure("ACTION_STALE", "surface action focused a pre-existing unrelated window")
        if not related_window_identities(initial_identity, frame["window"]):
            raise ControlFailure("ACTION_STALE", "surface action opened outside the bound WeChat process family")
    return surface_snapshot(frame)


def close_surface(request):
    frame = capture_surface_frame(request.get("expected_window_identity"))
    require_safe_surface_frame(frame)
    if frame["window"].get("process_kind") not in ("miniprogram", "web"):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "only a bound generic surface can be closed")
    confirmed = capture_surface_frame(frame["window_identity"])
    require_safe_surface_frame(confirmed)
    close_x11_window(frame["window"]["xid"])
    closing_instance = window_instance(frame["window"])
    deadline = time.monotonic() + 3.0
    while time.monotonic() < deadline:
        if closing_instance not in x11_wechat_window_inventory():
            return
        time.sleep(0.1)
    raise ControlFailure("ACTION_STALE", "surface close action did not close the bound window")


def capture_saved_account_confirmation():
    before_scope = active_surface_root()
    before_identity = verified_window_identity(before_scope)
    identity_digest = before_identity.get("digest")
    if before_identity.get("process_kind") != "wechat" or not (
        isinstance(identity_digest, str) and re.fullmatch(r"[0-9a-f]{64}", identity_digest)
    ):
        raise ControlFailure(
            "ACTION_STALE",
            "saved-account login is not in a verified official main WeChat window",
        )
    before_nodes = scope_nodes(before_scope)
    before_geometry = main_geometry(before_scope)
    before_target = saved_account_confirmation_target(before_nodes, before_geometry)
    if before_target is None:
        raise ControlFailure(
            "ACTION_STALE", "saved-account login confirmation is no longer available",
        )
    before_semantic = saved_account_page_semantic_signature(before_nodes, before_geometry)
    screenshot = capture_screenshot_png(before_identity["xid"])
    pixels, dimensions = screenshot_rgba(screenshot)
    require_screenshot_geometry(before_identity, dimensions)

    after_scope = active_surface_root()
    require_same_window(before_scope, after_scope)
    after_identity = verified_window_identity(after_scope)
    if before_identity != after_identity:
        raise ControlFailure(
            "ACTION_STALE", "saved-account login window changed while it was captured",
        )
    after_nodes = scope_nodes(after_scope)
    after_geometry = main_geometry(after_scope)
    after_target = saved_account_confirmation_target(after_nodes, after_geometry)
    if after_target is None:
        raise ControlFailure(
            "ACTION_STALE", "saved-account login confirmation changed while it was captured",
        )
    after_semantic = saved_account_page_semantic_signature(after_nodes, after_geometry)
    if before_semantic != after_semantic or before_target["signature"] != after_target["signature"]:
        raise ControlFailure(
            "ACTION_STALE", "saved-account login page changed while it was captured",
        )
    rendered_digest = rendered_frame_digest(pixels, dimensions)
    auth_generation = semantic_digest({
        "page_semantics": after_semantic,
        "rendered_frame": rendered_digest,
        "window_identity": identity_digest,
        "window_geometry": list(after_geometry),
    })
    return {
        "scope": after_scope,
        "scope_identity": scope_identity(after_scope),
        "identity_digest": identity_digest,
        "target": after_target,
        "page_semantics": after_semantic,
        "rendered_frame_sha256": rendered_digest,
        "auth_generation": auth_generation,
        "screenshot": screenshot,
        "screenshot_sha256": hashlib.sha256(screenshot).hexdigest(),
    }


def continue_saved_account_login(request):
    if set(request) != {"operation", "expected_auth_generation"}:
        raise ControlFailure(
            "CLIENT_INCOMPATIBLE",
            "saved-account login requires only operation and expected_auth_generation",
        )
    expected_generation = request.get("expected_auth_generation")
    if not isinstance(expected_generation, str) or not re.fullmatch(
        r"[0-9a-f]{64}", expected_generation,
    ):
        raise ControlFailure("CLIENT_INCOMPATIBLE", "invalid saved-account authentication generation")
    initial = capture_saved_account_confirmation()
    confirmed = capture_saved_account_confirmation()
    if (
        initial["auth_generation"] != expected_generation
        or confirmed["auth_generation"] != expected_generation
        or initial["identity_digest"] != confirmed["identity_digest"]
        or initial["scope_identity"] != confirmed["scope_identity"]
        or initial["target"]["signature"] != confirmed["target"]["signature"]
    ):
        raise ControlFailure(
            "ACTION_STALE", "saved-account login page changed before confirmation",
        )

    # Capture the active scope again immediately before the only side effect,
    # then resolve the login control afresh from its accessibility path.
    final = capture_saved_account_confirmation()
    if (
        final["auth_generation"] != expected_generation
        or confirmed["identity_digest"] != final["identity_digest"]
        or confirmed["scope_identity"] != final["scope_identity"]
        or confirmed["target"]["signature"] != final["target"]["signature"]
    ):
        raise ControlFailure(
            "ACTION_STALE", "saved-account login window changed before activation",
        )
    login = final["target"]["login"]
    if not path_is_within(login["path"], final["scope"][1]):
        raise ControlFailure("ACTION_STALE", "saved-account login control left its window")
    node = resolve_path(login["path"])
    if (
        not visible_node(node, main_geometry(final["scope"]))
        or observable_accessible_identity(node) != login["accessible_identity"]
        or node_signature(node, login["path"]) != login["static_signature"]
    ):
        raise ControlFailure("ACTION_STALE", "saved-account login control changed")
    interface = action_interface(node)
    action = saved_account_action_from_interface(interface)
    if action is None or action != login["action"]:
        raise ControlFailure("ACTION_STALE", "saved-account login action changed")
    try:
        accepted = interface.doAction(action["index"])
    except Exception as exc:
        raise ControlFailure(
            "ACTION_OUTCOME_UNCERTAIN",
            "saved-account login was dispatched but its outcome is unknown",
            consumed=True,
        ) from exc
    if not accepted:
        raise ControlFailure(
            "ACTION_OUTCOME_UNCERTAIN",
            "saved-account login was dispatched but the client did not confirm acceptance",
            consumed=True,
        )
    return {"ok": True, "consumed": True}


def dispatch(request):
    operation = request.get("operation")
    if operation == "probe":
        return probe()
    if operation == "submit_auth_code":
        submit_auth_code(request.get("text"))
        return {"ok": True}
    if operation == SAVED_ACCOUNT_AUTH_ACTION_ID:
        return continue_saved_account_login(request)
    if operation == "snapshot_surface":
        if not request.get("expected_window_identity"):
            raise ControlFailure("ACTION_STALE", "expected surface window identity is required")
        return {"ok": True, "surface": surface_snapshot(
            expected_window_identity=request.get("expected_window_identity"),
        )}
    if operation == "act_surface":
        return {"ok": True, "surface": act_surface(request)}
    if operation == "close_surface":
        if not request.get("expected_window_identity"):
            raise ControlFailure("ACTION_STALE", "expected surface window identity is required")
        close_surface(request)
        return {"ok": True}
    state = probe()
    if state.get("state") == "AUTH_REQUIRED":
        raise ControlFailure("AUTH_REQUIRED", "WeChat login is required")
    if state.get("state") != "ONLINE":
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
    if operation == "open_named_surface":
        return {"ok": True, "surface": open_named_surface(request)}
    raise ControlFailure("CLIENT_INCOMPATIBLE", "unknown semantic operation")


def main():
    try:
        emit(dispatch(read_request()))
    except ControlFailure as exc:
        response = {"ok": False, "code": exc.code, "error": str(exc)}
        if exc.consumed:
            response["consumed"] = True
        emit(response)
    except subprocess.CalledProcessError:
        emit({"ok": False, "code": "CLIENT_INCOMPATIBLE", "error": "desktop helper failed"})
    except Exception as exc:
        emit({"ok": False, "code": "CLIENT_INCOMPATIBLE", "error": f"accessibility failure: {type(exc).__name__}"})


if __name__ == "__main__":
    main()
