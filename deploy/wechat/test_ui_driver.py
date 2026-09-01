#!/usr/bin/env python3
"""Unit tests for fail-closed WeChat accessibility target selection."""

import importlib.util
import contextlib
import hashlib
from pathlib import Path
import sys
import types
import unittest
from unittest import mock


def load_driver():
    stub = types.ModuleType("pyatspi")
    stub.DESKTOP_COORDS = 0
    stub.STATE_ACTIVE = "active"
    stub.STATE_EDITABLE = "editable"
    stub.STATE_FOCUSED = "focused"
    stub.STATE_SELECTED = "selected"
    stub.STATE_SHOWING = "showing"
    stub.STATE_VISIBLE = "visible"
    stub.Registry = types.SimpleNamespace(getDesktop=lambda _index: None)
    sys.modules["pyatspi"] = stub
    path = Path(__file__).with_name("ui_driver.py")
    spec = importlib.util.spec_from_file_location("wechat_ui_driver", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


driver = load_driver()


class FakeState:
    def __init__(self, values):
        self.values = values

    def contains(self, value):
        return value in self.values


class FakeAction:
    def __init__(self, node, names):
        self.node = node
        self.names = list(names)
        self.nActions = len(self.names)

    def getName(self, index):
        return self.names[index]

    def doAction(self, index):
        self.node.activated.append(index)
        return True


class FakeComponent:
    def __init__(self, node):
        self.node = node

    def getExtents(self, _coordinates):
        return types.SimpleNamespace(
            x=self.node.geometry[0], y=self.node.geometry[1],
            width=self.node.geometry[2], height=self.node.geometry[3],
        )

    def grabFocus(self):
        self.node.focused = True
        return True


class FakeEditable:
    def __init__(self, node):
        self.node = node

    def setTextContents(self, value):
        self.node.text = value
        return True


class FakeText:
    def __init__(self, node):
        self.node = node

    @property
    def characterCount(self):
        return len(self.node.text)

    def getText(self, start, end):
        return self.node.text[start:end]


class FakeNode:
    next_accessible_id = 1

    def __init__(
        self, name="", role="label", geometry=(0, 0, 20, 20), *,
        description="", states=("showing", "visible"), actions=(), editable=False, readable=True,
        children=(), text="", accessible_path=None, accessible_bus=":fixture.1",
        accessible_process_id=42,
    ):
        self.name = name
        self.description = description
        self.role = role
        self.geometry = geometry
        self.states = set(states)
        self.action_names = list(actions)
        self.editable = editable
        self.readable = readable
        self.children = list(children)
        self.text = text
        self.activated = []
        self.focused = False
        if accessible_path is None:
            accessible_path = f"/fixture/accessible/{FakeNode.next_accessible_id}"
            FakeNode.next_accessible_id += 1
        self.path = accessible_path
        self.app = types.SimpleNamespace(bus_name=accessible_bus)
        self.accessible_process_id = accessible_process_id

    @property
    def childCount(self):
        return len(self.children)

    def getChildAtIndex(self, index):
        return self.children[index]

    def getRoleName(self):
        return self.role

    def get_process_id(self):
        return self.accessible_process_id

    def getState(self):
        return FakeState(self.states)

    def queryAction(self):
        if not self.action_names:
            raise RuntimeError("no action")
        return FakeAction(self, self.action_names)

    def queryComponent(self):
        return FakeComponent(self)

    def queryEditableText(self):
        if not self.editable:
            raise RuntimeError("not editable")
        return FakeEditable(self)

    def queryText(self):
        if not self.readable:
            raise RuntimeError("text hidden")
        return FakeText(self)


def install_tree(*children):
    window = FakeNode(
        "WeChat", "window", (0, 0, 1000, 800),
        states=("active", "showing", "visible"), children=children,
    )
    root = FakeNode("Desktop", "desktop", (0, 0, 1200, 900), children=(window,))
    driver.desktop = lambda: root
    return root, window


def install_online_tree(*children, window_geometry=(0, 0, 1000, 800)):
    x, y, width, height = window_geometry
    search = FakeNode(
        "Search", "entry", (x + 20, y + 20, min(220, width // 2), 36), editable=True,
    )
    chats = FakeNode(
        "Chats", "push button", (x + 12, y + 90, 72, 36), actions=("click",),
    )
    contacts = FakeNode(
        "Contacts", "push button", (x + 12, y + 140, 72, 36), actions=("click",),
    )
    navigation = FakeNode(
        "Navigation", "panel", (x + 5, y + 70, 90, min(180, height - 80)),
        children=(chats, contacts),
    )
    root, window = install_tree(search, navigation, *children)
    window.geometry = window_geometry
    return root, window


def fake_surface_frame(window, ocr=(), screenshot_sha256=None, process_kind="miniprogram"):
    scope = (window, (0,))
    nodes = driver.visible_scope_nodes(scope)
    screenshot_sha256 = screenshot_sha256 or ("a" * 64)
    identity = {
        "version": 1,
        "xid": "0x1234",
        "pid": 42,
        "pid_starttime": 123456,
        "process_kind": process_kind,
        "atspi_pid": 42,
        "atspi_xid": "0x1234",
        "lineage": [{"pid": 42, "starttime": 123456}],
        "wm_class": ["wechat", "wechat"],
        "geometry": [0, 0, 1000, 800],
        "scope_geometry": [0, 0, 1000, 800],
    }
    identity["digest"] = driver.semantic_digest(identity)
    pixel = int(screenshot_sha256[:2], 16)
    image_width, image_height = identity["geometry"][2:]
    pixels = bytes((pixel, pixel, pixel, 255)) * (image_width * image_height)
    return {
        "scope": scope,
        "nodes": nodes,
        "screenshot": b"\x89PNG\r\n\x1a\nfixture",
        "screenshot_rgba": pixels,
        "screenshot_dimensions": (image_width, image_height),
        "screenshot_sha256": screenshot_sha256,
        "rendered_frame_sha256": driver.rendered_frame_digest(
            pixels, (image_width, image_height),
        ),
        "ocr": list(ocr),
        "generation": driver.frame_semantic_generation(window, (0,), nodes, ocr),
        "window": identity,
        "window_identity": driver.encode_window_identity(identity),
    }


def frame_locator_fields(frame):
    return {
        "surface_generation_value": frame["generation"],
        "screenshot_sha256": frame["screenshot_sha256"],
        "rendered_frame_sha256": frame["rendered_frame_sha256"],
        "scope_identity_value": driver.scope_identity(frame["scope"]),
        "scope_geometry": driver.main_geometry(frame["scope"]),
        "window_identity_value": frame["window_identity"],
    }


def message_locator(frame, node, path=(0, 0), action=0):
    geometry = driver.bounds(node)
    return driver.make_locator(
        path, "message_surface", action, node=node, source="atspi",
        bounds_value=geometry, strict_frame=True,
        rendered_region_sha256=driver.screenshot_region_digest(frame, geometry),
        accessible_identity_value=driver.observable_accessible_identity(node),
        **frame_locator_fields(frame),
    )


def verified_wechat_identity(digest_character="a", geometry=(200, 100, 420, 600)):
    return {
        "process_kind": "wechat",
        "digest": digest_character * 64,
        "xid": "0x1234",
        "geometry": list(geometry),
    }


def fixture_pixels(dimensions=(420, 600), value=0x22):
    return bytes((value, value, value, 255)) * (dimensions[0] * dimensions[1])


@contextlib.contextmanager
def saved_account_screenshot(dimensions=(420, 600), pixels=None, png=b"saved-account-png"):
    pixels = pixels if pixels is not None else fixture_pixels(dimensions)
    with mock.patch.object(driver, "capture_screenshot_png", return_value=png):
        with mock.patch.object(driver, "screenshot_rgba", return_value=(pixels, dimensions)):
            yield {"png": png, "pixels": pixels, "dimensions": dimensions}


def expected_saved_auth_generation(
    window, pixels, dimensions=(420, 600), identity_digest="a" * 64,
):
    scope = (window, (0,))
    nodes = driver.scope_nodes(scope)
    page_semantics = driver.saved_account_page_semantic_signature(
        nodes, driver.main_geometry(scope),
    )
    return driver.semantic_digest({
        "page_semantics": page_semantics,
        "rendered_frame": driver.rendered_frame_digest(pixels, dimensions),
        "window_identity": identity_digest,
        "window_geometry": list(driver.main_geometry(scope)),
    })


class TargetSelectionTests(unittest.TestCase):
    def run_without_sleep(self, callback):
        original = driver.time.sleep
        driver.time.sleep = lambda _seconds: None
        try:
            return callback()
        finally:
            driver.time.sleep = original

    def test_official_appimage_name_counts_as_running(self):
        cmdline = b"/opt/wechat/WeChat.AppImage\x00--no-sandbox\x00"
        process = driver.Path("/proc/424242")
        with mock.patch.object(driver.Path, "iterdir", return_value=[process]):
            with mock.patch.object(driver.Path, "read_bytes", autospec=True, return_value=cmdline) as read_bytes:
                self.assertTrue(driver.process_running())
        read_bytes.assert_called_once_with(process / "cmdline")

    def test_duplicate_send_buttons_are_ambiguous_without_clicking(self):
        first = FakeNode("Send", "push button", (700, 700, 80, 30), actions=("click",))
        second = FakeNode("Send", "push button", (800, 700, 80, 30), actions=("click",))
        _root, window = install_tree(first, second)

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.click_named(driver.SEND_LABELS, (window, (0,)), purpose="send button")

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)
        self.assertEqual([], first.activated)
        self.assertEqual([], second.activated)

    def test_explicitly_named_non_activation_action_is_never_a_default_click(self):
        deceptive = FakeNode(
            "Open", "push button", (100, 100, 120, 40), actions=("deactivate",),
        )
        self.assertIsNone(driver.preferred_action_index(deceptive))
        with self.assertRaises(driver.ControlFailure) as raised:
            driver.activate(deceptive)
        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        self.assertEqual([], deceptive.activated)

    def test_duplicate_actions_on_one_target_are_ambiguous(self):
        button = FakeNode("Send", "push button", actions=("click", "press"))

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.activate(button)

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)
        self.assertEqual([], button.activated)

    def test_chat_editor_must_be_unique_in_right_lower_pane(self):
        header = FakeNode("Alice", "label", (600, 40, 100, 30))
        first = FakeNode("", "entry", (480, 560, 400, 120), editable=True)
        second = FakeNode("", "entry", (500, 590, 350, 100), editable=True)
        _root, window = install_tree(header, first, second)

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.unique_chat_editor((window, (0,)), "Alice")

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)

    def test_auth_ambiguity_is_detected_before_mutating_code_field(self):
        marker = FakeNode("短信验证码", "label", (300, 180, 180, 30))
        editor = FakeNode("验证码", "entry", (300, 240, 200, 40), editable=True)
        first = FakeNode("Confirm", "push button", (300, 320, 80, 30), actions=("click",))
        second = FakeNode("OK", "push button", (400, 320, 80, 30), actions=("click",))
        install_tree(marker, editor, first, second)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            with self.assertRaises(driver.ControlFailure) as raised:
                driver.submit_auth_code("123456")

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)
        self.assertEqual("", editor.text)

    def test_auth_code_is_never_entered_into_a_miniprogram_form(self):
        marker = FakeNode("短信验证码", "label", (300, 180, 180, 30))
        editor = FakeNode("验证码", "entry", (300, 240, 200, 40), editable=True)
        confirm = FakeNode("Confirm", "push button", (300, 320, 80, 30), actions=("click",))
        install_tree(marker, editor, confirm)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "miniprogram"},
        ):
            with self.assertRaises(driver.ControlFailure) as raised:
                driver.submit_auth_code("123456")

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        self.assertEqual("", editor.text)
        self.assertEqual([], confirm.activated)

    def test_message_body_mismatch_fails_closed(self):
        editor = FakeNode("", "entry", editable=True, text="different")

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.verify_text_contents(editor, "expected", timeout=0)

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)

    def test_keyboard_text_fallback_rejects_focus_owned_by_another_editor(self):
        target = FakeNode(
            "Target", "entry", (500, 500, 300, 80),
            states=("showing", "visible", "editable"), editable=False,
        )
        other = FakeNode(
            "Other", "entry", (500, 300, 300, 80),
            states=("showing", "visible", "focused"), editable=True,
        )
        _root, window = install_tree(target, other)
        component = FakeComponent(target)
        component.grabFocus = lambda: True
        target.queryComponent = lambda: component
        scope = (window, (0,))
        identity = fake_surface_frame(window)["window"]

        with mock.patch.object(driver, "active_surface_root", return_value=scope):
            with mock.patch.object(driver, "verified_window_identity", return_value=identity):
                with mock.patch.object(driver.subprocess, "run") as process:
                    with mock.patch.object(driver, "send_key") as send_key:
                        with self.assertRaises(driver.ControlFailure) as raised:
                            driver.set_text(target, "secret", scope, (0, 0))

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        process.assert_not_called()
        send_key.assert_not_called()

    def test_direct_editable_write_rejects_an_x11_window_switch(self):
        editor = FakeNode("Target", "entry", (500, 500, 300, 80), editable=True)
        _root, window = install_tree(editor)
        scope = (window, (0,))

        with mock.patch.object(driver, "active_surface_root", return_value=scope):
            with mock.patch.object(
                driver, "verified_window_identity",
                side_effect=({"digest": "before"}, {"digest": "after"}),
            ):
                with self.assertRaises(driver.ControlFailure) as raised:
                    driver.set_text(editor, "secret", scope, (0, 0))

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual("", editor.text)

    def test_attachment_paste_aborts_before_clipboard_when_focus_is_unproven(self):
        editor = FakeNode("Target", "entry", editable=True)
        _root, window = install_tree(editor)
        scope = (window, (0,))
        failure = driver.ControlFailure("CLIENT_INCOMPATIBLE", "no focus")

        with mock.patch.object(driver, "validate_attachment_paths", return_value=[Path("/tmp/item")]):
            with mock.patch.object(driver, "focus_editor_for_keyboard", side_effect=failure):
                with mock.patch.object(driver.subprocess, "run") as process:
                    with mock.patch.object(driver, "send_key") as send_key:
                        with self.assertRaises(driver.ControlFailure):
                            driver.stage_clipboard_files(["/tmp/item"], editor, (0, 0), scope)

        process.assert_not_called()
        send_key.assert_not_called()

    def test_attachment_paste_rechecks_active_window_after_loading_clipboard(self):
        editor = FakeNode("Target", "entry", editable=True)
        _root, window = install_tree(editor)
        scope = (window, (0,))
        expected_window = {"digest": "bound"}
        failure = driver.ControlFailure("ACTION_STALE", "window changed")

        with mock.patch.object(driver, "validate_attachment_paths", return_value=[Path("/tmp/item")]):
            with mock.patch.object(
                driver, "focus_editor_for_keyboard",
                return_value=(scope, "editor-signature", expected_window),
            ):
                with mock.patch.object(driver, "revalidate_keyboard_target", side_effect=failure):
                    with mock.patch.object(driver.subprocess, "run") as process:
                        with mock.patch.object(driver, "send_key") as send_key:
                            with self.assertRaises(driver.ControlFailure):
                                driver.stage_clipboard_files(["/tmp/item"], editor, (0, 0), scope)

        self.assertEqual(2, process.call_count)
        send_key.assert_not_called()

    def test_send_uses_verified_editor_and_unique_chat_button(self):
        conversation = FakeNode(
            "Alice", "list item", (20, 100, 300, 50), actions=("click",),
        )
        conversation_list = FakeNode(
            "Chats", "list", (0, 60, 350, 700), children=(conversation,),
        )
        header = FakeNode("Alice", "label", (600, 40, 100, 30))
        editor = FakeNode("", "entry", (480, 560, 400, 120), editable=True)
        send = FakeNode("Send", "push button", (800, 700, 80, 30), actions=("click",))
        install_tree(conversation_list, header, editor, send)
        locator = driver.make_locator((0, 0, 0), "conversation", 0, node=conversation)

        with mock.patch.object(driver, "verified_window_identity", return_value={"process_kind": "wechat"}):
            self.run_without_sleep(lambda: driver.send_message({
                "conversation_title": "Alice",
                "conversation_locator": locator,
                "text": "verified body",
            }))

        self.assertEqual("verified body", editor.text)
        self.assertEqual([0], conversation.activated)
        self.assertEqual([0], send.activated)

    def test_unreadable_message_editor_prevents_send(self):
        conversation = FakeNode(
            "Alice", "list item", (20, 100, 300, 50), actions=("click",),
        )
        conversation_list = FakeNode(
            "Chats", "list", (0, 60, 350, 700), children=(conversation,),
        )
        header = FakeNode("Alice", "label", (600, 40, 100, 30))
        editor = FakeNode(
            "", "entry", (480, 560, 400, 120), editable=True, readable=False,
        )
        send = FakeNode("Send", "push button", (800, 700, 80, 30), actions=("click",))
        install_tree(conversation_list, header, editor, send)
        locator = driver.make_locator((0, 0, 0), "conversation", 0, node=conversation)

        with mock.patch.object(driver, "verified_window_identity", return_value={"process_kind": "wechat"}):
            with self.assertRaises(driver.ControlFailure) as raised:
                self.run_without_sleep(lambda: driver.send_message({
                    "conversation_title": "Alice",
                    "conversation_locator": locator,
                    "text": "must be readable",
                }))

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        self.assertEqual([], send.activated)

    def test_surface_locator_rejects_reused_path_after_page_change(self):
        share = FakeNode("Share", "push button", (700, 100, 80, 30), actions=("click",))
        _root, window = install_tree(share)
        frame = fake_surface_frame(window)
        encoded = driver.make_locator(
            (0, 0), "share", 0, node=share,
            source="atspi", strict_frame=True, **frame_locator_fields(frame),
        )
        locator = driver.decode_locator(encoded)
        with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
            validated, _scope = driver.validate_surface_locator(
                locator, expected_kind="share", require_unique_kind=True,
            )
        self.assertEqual("Share", validated.name)

        share.name = "Different"
        changed = fake_surface_frame(window)
        with self.assertRaises(driver.ControlFailure) as raised:
            with mock.patch.object(driver, "capture_surface_frame", return_value=changed):
                driver.validate_surface_locator(
                    locator, expected_kind="share", require_unique_kind=True,
                )
        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], share.activated)

    def test_duplicate_share_actions_are_ambiguous(self):
        first = FakeNode("Share", "push button", (700, 100, 80, 30), actions=("click",))
        second = FakeNode("Share", "push button", (800, 100, 80, 30), actions=("click",))
        _root, window = install_tree(first, second)
        frame = fake_surface_frame(window)
        locator = driver.decode_locator(driver.make_locator(
            (0, 0), "share", 0, node=first,
            source="atspi", strict_frame=True, **frame_locator_fields(frame),
        ))

        with self.assertRaises(driver.ControlFailure) as raised:
            with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
                driver.validate_surface_locator(
                    locator, expected_kind="share", require_unique_kind=True,
                )

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)
        self.assertEqual([], first.activated)
        self.assertEqual([], second.activated)

    def test_square_post_image_does_not_make_online_client_auth_required(self):
        image = FakeNode("post photo", "image", (400, 200, 180, 180))
        install_online_tree(image)

        with mock.patch.object(driver, "verified_window_identity", return_value={"process_kind": "wechat"}):
            self.assertEqual("ONLINE", driver.probe()["state"])

    def test_probe_does_not_infer_online_from_an_empty_official_window(self):
        install_tree()

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("DEGRADED", state["state"])

    def test_nested_main_navigation_is_positive_online_evidence(self):
        search = FakeNode("Search", "entry", (20, 20, 220, 36), editable=True)
        chats = FakeNode(
            "Chats", "push button", (12, 100, 72, 36), actions=("click",),
        )
        contacts = FakeNode(
            "Contacts", "push button", (12, 160, 72, 36), actions=("click",),
        )
        chats_wrapper = FakeNode(
            "", "filler", (8, 90, 80, 50), children=(chats,),
        )
        contacts_wrapper = FakeNode(
            "", "filler", (8, 150, 80, 50), children=(contacts,),
        )
        navigation = FakeNode(
            "Navigation", "panel", (5, 70, 90, 180),
            children=(chats_wrapper, contacts_wrapper),
        )
        install_tree(search, navigation)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            self.assertEqual("ONLINE", driver.probe()["state"])

    def test_unrelated_navigation_words_do_not_prove_online(self):
        search = FakeNode("Search", "entry", (20, 20, 220, 36), editable=True)
        chats = FakeNode(
            "Chats", "push button", (12, 100, 72, 36), actions=("click",),
        )
        contacts = FakeNode(
            "Contacts", "push button", (700, 160, 90, 36), actions=("click",),
        )
        left = FakeNode("Post actions", "panel", (5, 70, 100, 180), children=(chats,))
        right = FakeNode("Article actions", "panel", (680, 130, 140, 100), children=(contacts,))
        install_tree(search, left, right)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            self.assertEqual("DEGRADED", driver.probe()["state"])

    def test_login_text_and_square_qr_are_auth_required(self):
        marker = FakeNode("微信登录", "label", (350, 100, 160, 30))
        image = FakeNode("二维码", "image", (400, 200, 180, 180))
        install_tree(marker, image)

        with mock.patch.object(driver, "verified_window_identity", return_value={"process_kind": "wechat"}):
            state = driver.probe()
        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertEqual({"x": 400, "y": 200, "width": 180, "height": 180}, state["qr_bounds"])

    def test_qr_specific_prompt_accepts_an_unlabelled_square_image(self):
        marker = FakeNode("Scan QR", "label", (350, 100, 160, 30))
        image = FakeNode("", "image", (400, 200, 180, 180))
        install_tree(marker, image)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertEqual("qr", state["auth_kind"])
        self.assertEqual({"x": 400, "y": 200, "width": 180, "height": 180}, state["qr_bounds"])

    def test_qr_bounds_ignore_hidden_and_out_of_window_candidates(self):
        marker = FakeNode("Scan QR", "label", (350, 100, 160, 30))
        image = FakeNode("", "image", (400, 200, 180, 180))
        hidden = FakeNode(
            "QR code", "image", (300, 400, 300, 300), states=("visible",),
        )
        outside = FakeNode("QR code", "image", (900, 500, 300, 300))
        install_tree(marker, image, hidden, outside)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertEqual({"x": 400, "y": 200, "width": 180, "height": 180}, state["qr_bounds"])

    def test_generic_login_title_and_avatar_are_not_qr_evidence(self):
        marker = FakeNode("WeChat Login", "label", (300, 100, 220, 30))
        avatar = FakeNode("profile avatar", "image", (350, 190, 120, 120))
        install_tree(marker, avatar)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("DEGRADED", state["state"])
        self.assertNotIn("qr_bounds", state)

    def test_hidden_or_discursive_qr_content_is_not_authentication(self):
        cases = (
            (
                FakeNode("扫码登录", "label", (350, 100, 160, 30), states=("visible",)),
                FakeNode("二维码", "image", (400, 200, 180, 180), states=("visible",)),
            ),
            (
                FakeNode("微信登录问题和验证码说明", "label", (350, 100, 260, 30)),
                FakeNode("二维码示例", "image", (400, 200, 180, 180)),
            ),
        )
        for marker, image in cases:
            with self.subTest(marker=marker.name):
                install_online_tree(marker, image)
                with mock.patch.object(
                    driver, "verified_window_identity", return_value={"process_kind": "wechat"},
                ):
                    state = driver.probe()
                self.assertEqual("ONLINE", state["state"])
                self.assertNotIn("qr_bounds", state)

    def test_miniprogram_login_like_qr_is_not_account_authentication(self):
        marker = FakeNode("微信登录", "label", (350, 100, 160, 30))
        image = FakeNode("二维码", "image", (400, 200, 180, 180))
        install_tree(marker, image)
        with mock.patch.object(driver, "verified_window_identity", return_value={"process_kind": "miniprogram"}):
            state = driver.probe()
        self.assertEqual("DEGRADED", state["state"])
        self.assertNotIn("qr_bounds", state)

    def test_post_discussing_login_or_codes_is_not_an_auth_surface(self):
        post = FakeNode("微信登录问题和验证码说明", "paragraph", (100, 120, 500, 80))
        _root, window = install_online_tree(post)

        with mock.patch.object(driver, "verified_window_identity", return_value={"process_kind": "wechat"}):
            self.assertEqual("ONLINE", driver.probe()["state"])
        surface = driver.surface_snapshot(fake_surface_frame(window))
        self.assertIn("微信登录问题和验证码说明", surface["semantic_text"])

    def test_compact_saved_account_confirmation_is_auth_required_without_clicking(self):
        for login_label, alternative, user_label in (
            ("Log In", "Switch Account", "Current UserAlice"),
            ("Log In", "Transfer files only", "Current UserBob"),
            ("登录", "切换账号", "当前用户小明"),
            ("登录", "仅传输文件", "当前用户小红"),
            ("登录", "仅传文件", "当前用户测试账号"),
        ):
            with self.subTest(login=login_label, alternative=alternative):
                nodes = [
                    FakeNode(user_label, "label", (300, 145, 220, 30)),
                    FakeNode("profile avatar", "image", (350, 190, 120, 120)),
                    FakeNode(login_label, "push button", (290, 380, 240, 50), actions=("click",)),
                    FakeNode(alternative, "push button", (290, 455, 240, 40), actions=("click",)),
                ]
                _root, window = install_tree(*nodes)
                window.geometry = (200, 100, 420, 600)

                with saved_account_screenshot() as screenshot:
                    with mock.patch.object(
                        driver, "verified_window_identity",
                        return_value=verified_wechat_identity(),
                    ):
                        state = driver.probe()

                self.assertEqual("AUTH_REQUIRED", state["state"])
                self.assertEqual("phone_confirmation", state["auth_kind"])
                self.assertFalse(state["can_submit_code"])
                self.assertIsNone(state["qr_bounds"])
                generation = state["auth_generation"]
                self.assertRegex(generation, r"^[0-9a-f]{64}$")
                self.assertEqual(
                    [driver.saved_account_auth_action(generation)], state["actions"],
                )
                self.assertEqual(
                    hashlib.sha256(screenshot["png"]).hexdigest(), state["screenshot_sha256"],
                )
                for node in nodes:
                    self.assertEqual([], node.activated)

    def test_real_compact_side_by_side_saved_account_layout_is_actionable(self):
        geometry = (200, 100, 291, 396)
        user = FakeNode("Current UserAlice", "label", (245, 150, 200, 28))
        login = FakeNode(
            "Log In", "push button", (275, 285, 140, 44), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (215, 350, 125, 38), actions=("click",),
        )
        transfer = FakeNode(
            "Transfer files only", "push button", (350, 350, 125, 38), actions=("click",),
        )
        _root, window = install_tree(user, login, switch, transfer)
        window.geometry = geometry

        with saved_account_screenshot(dimensions=(291, 396)):
            with mock.patch.object(
                driver, "verified_window_identity",
                return_value=verified_wechat_identity(geometry=geometry),
            ):
                state = driver.probe()

        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertEqual("phone_confirmation", state["auth_kind"])
        self.assertEqual(1, len(state["actions"]))
        self.assertTrue(state["actions"][0]["image_bound"])
        self.assertEqual([], login.activated)

    def test_real_setfocus_saved_account_layout_uses_a_confirmed_visual_action(self):
        geometry = (200, 100, 291, 396)
        user = FakeNode("Current UserAlice", "label", (245, 150, 200, 28))
        login = FakeNode(
            "Log In", "push button", (275, 285, 140, 44), actions=("SetFocus",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (215, 350, 125, 38), actions=("SetFocus",),
        )
        transfer = FakeNode(
            "Transfer files only", "push button", (350, 350, 125, 38), actions=("SetFocus",),
        )
        _root, window = install_tree(user, login, switch, transfer)
        window.geometry = geometry

        with saved_account_screenshot(dimensions=(291, 396)):
            with mock.patch.object(
                driver, "verified_window_identity",
                return_value=verified_wechat_identity(geometry=geometry),
            ):
                state = driver.probe()

        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertEqual("phone_confirmation", state["auth_kind"])
        self.assertEqual(1, len(state["actions"]))
        self.assertTrue(state["actions"][0]["image_bound"])
        self.assertEqual([], login.activated)

    def test_saved_account_title_and_avatar_never_publish_qr_bounds(self):
        title = FakeNode("WeChat Login", "label", (300, 115, 220, 28))
        user = FakeNode("Current UserAlice", "label", (300, 155, 220, 28))
        avatar = FakeNode("profile avatar", "image", (350, 200, 120, 120))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(title, user, avatar, login, switch)
        window.geometry = (200, 100, 420, 600)

        with saved_account_screenshot():
            with mock.patch.object(
                driver, "verified_window_identity", return_value=verified_wechat_identity(),
            ):
                state = driver.probe()

        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertEqual("phone_confirmation", state["auth_kind"])
        self.assertIsNone(state["qr_bounds"])

    def test_saved_account_without_bound_user_label_does_not_advertise_action(self):
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(login, switch)
        window.geometry = (200, 100, 420, 600)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("DEGRADED", state["state"])
        self.assertNotIn("actions", state)

    def test_saved_account_jump_buttons_are_auth_only_and_never_actionable(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("jump",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("jump",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertNotIn("actions", state)
        self.assertEqual([], login.activated)

    def test_saved_account_without_accessible_object_identity_has_no_action(self):
        user = FakeNode(
            "Current UserAlice", "label", (300, 145, 220, 30), accessible_path="",
        )
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50),
            actions=("click",), accessible_path="",
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40),
            actions=("click",), accessible_path="",
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertNotIn("actions", state)

    def test_saved_account_out_of_bounds_or_overlapping_controls_are_not_actionable(self):
        for login_geometry, alternative_geometry in (
            ((100, 380, 240, 50), (290, 455, 240, 40)),
            ((290, 380, 240, 50), (300, 400, 220, 40)),
        ):
            with self.subTest(login=login_geometry, alternative=alternative_geometry):
                user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
                login = FakeNode(
                    "Log In", "push button", login_geometry, actions=("click",),
                )
                switch = FakeNode(
                    "Switch Account", "push button", alternative_geometry, actions=("click",),
                )
                _root, window = install_tree(user, login, switch)
                window.geometry = (200, 100, 420, 600)

                with mock.patch.object(
                    driver, "verified_window_identity", return_value={"process_kind": "wechat"},
                ):
                    state = driver.probe()

                self.assertEqual("DEGRADED", state["state"])
                self.assertNotIn("actions", state)
                self.assertEqual([], login.activated)

    def test_saved_account_words_in_a_compact_post_are_not_authentication(self):
        post_nodes = (
            FakeNode("Current User", "text", (300, 150, 180, 30)),
            FakeNode("Log In", "paragraph", (290, 380, 240, 50)),
            FakeNode("Switch Account", "paragraph", (290, 455, 240, 40)),
            FakeNode("Transfer files only", "paragraph", (290, 510, 240, 40)),
        )
        _root, window = install_online_tree(
            *post_nodes, window_geometry=(200, 100, 420, 600),
        )

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            self.assertEqual("DEGRADED", driver.probe()["state"])
        surface = driver.surface_snapshot(fake_surface_frame(window))
        self.assertIn("Transfer files only", surface["semantic_text"])

    def test_saved_account_links_in_a_compact_post_are_not_authentication(self):
        login_link = FakeNode(
            "Log In", "link", (290, 380, 240, 50), actions=("jump",),
        )
        switch_link = FakeNode(
            "Switch Account", "link", (290, 455, 240, 40), actions=("jump",),
        )
        _root, window = install_online_tree(
            login_link, switch_link, window_geometry=(200, 100, 420, 600),
        )

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("DEGRADED", state["state"])
        self.assertNotIn("actions", state)
        self.assertEqual([], login_link.activated)
        self.assertEqual([], switch_link.activated)

    def test_saved_account_controls_in_a_large_window_are_not_authentication(self):
        login = FakeNode(
            "Log In", "push button", (500, 480, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (500, 555, 240, 40), actions=("click",),
        )
        install_online_tree(login, switch)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("ONLINE", state["state"])
        self.assertEqual([], login.activated)
        self.assertEqual([], switch.activated)

    def test_saved_account_confirmation_requires_unique_controls(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        first_login = FakeNode(
            "Log In", "push button", (290, 355, 240, 45), actions=("click",),
        )
        second_login = FakeNode(
            "Log In", "push button", (290, 410, 240, 45), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 475, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, first_login, second_login, switch)
        window.geometry = (200, 100, 420, 600)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()
        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertNotIn("actions", state)
        self.assertEqual([], first_login.activated)
        self.assertEqual([], second_login.activated)
        self.assertEqual([], switch.activated)

    def test_saved_account_confirmation_allows_both_unique_alternative_controls(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 355, 240, 45), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 430, 240, 40), actions=("click",),
        )
        transfer = FakeNode(
            "Transfer files only", "push button", (290, 485, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, switch, transfer)
        window.geometry = (200, 100, 420, 600)

        with saved_account_screenshot():
            with mock.patch.object(
                driver, "verified_window_identity", return_value=verified_wechat_identity(),
            ):
                state = driver.probe()
        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertEqual("phone_confirmation", state["auth_kind"])
        self.assertEqual(
            [driver.saved_account_auth_action(state["auth_generation"])], state["actions"],
        )
        self.assertEqual([], login.activated)
        self.assertEqual([], switch.activated)
        self.assertEqual([], transfer.activated)

    def test_saved_account_confirmation_rejects_duplicate_semantic_alternatives(self):
        user = FakeNode("当前用户小明", "label", (300, 145, 220, 30))
        login = FakeNode(
            "登录", "push button", (290, 355, 240, 45), actions=("click",),
        )
        english_switch = FakeNode(
            "Switch Account", "push button", (290, 430, 240, 40), actions=("click",),
        )
        chinese_switch = FakeNode(
            "切换账号", "push button", (290, 485, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, english_switch, chinese_switch)
        window.geometry = (200, 100, 420, 600)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()
        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertNotIn("actions", state)
        self.assertEqual([], login.activated)
        self.assertEqual([], english_switch.activated)
        self.assertEqual([], chinese_switch.activated)

    def test_continue_saved_account_login_revalidates_then_clicks_only_login(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))

        with saved_account_screenshot() as screenshot:
            generation = expected_saved_auth_generation(
                window, screenshot["pixels"], screenshot["dimensions"],
            )
            with mock.patch.object(driver, "active_surface_root", return_value=scope):
                with mock.patch.object(
                    driver, "verified_window_identity", return_value=verified_wechat_identity(),
                ) as verify:
                    result = driver.dispatch({
                        "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                        "expected_auth_generation": generation,
                    })

        self.assertEqual({"ok": True, "consumed": True}, result)
        self.assertGreaterEqual(verify.call_count, 6)
        self.assertEqual([0], login.activated)
        self.assertEqual([], switch.activated)

    def test_continue_setfocus_saved_account_login_uses_only_bound_pointer_action(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("SetFocus",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("SetFocus",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))
        identity = verified_wechat_identity()

        with saved_account_screenshot() as screenshot:
            generation = expected_saved_auth_generation(
                window, screenshot["pixels"], screenshot["dimensions"],
            )
            with mock.patch.object(driver, "active_surface_root", return_value=scope):
                with mock.patch.object(
                    driver, "verified_window_identity", return_value=identity,
                ):
                    with mock.patch.object(driver, "visual_pointer_action") as pointer:
                        result = driver.dispatch({
                            "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                            "expected_auth_generation": generation,
                        })

        self.assertEqual({"ok": True, "consumed": True}, result)
        self.assertEqual([], login.activated)
        self.assertEqual([], switch.activated)
        pointer.assert_called_once_with(
            (290, 380, 240, 50), 1,
            expected_window_identity=driver.encode_window_identity(identity),
            expected_rendered_frame_sha256=driver.rendered_frame_digest(
                screenshot["pixels"], screenshot["dimensions"],
            ),
        )

    def test_unknown_saved_account_action_is_not_visualized(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("SetValue",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("SetValue",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)

        with mock.patch.object(
            driver, "verified_window_identity", return_value={"process_kind": "wechat"},
        ):
            state = driver.probe()

        self.assertEqual("AUTH_REQUIRED", state["state"])
        self.assertNotIn("actions", state)
        self.assertEqual([], login.activated)

    def test_continue_saved_account_login_rejects_locator_before_capture(self):
        with mock.patch.object(driver, "active_surface_root") as capture:
            with self.assertRaises(driver.ControlFailure) as raised:
                driver.dispatch({
                    "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                    "locator": "untrusted",
                    "expected_auth_generation": "a" * 64,
                })

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        self.assertFalse(raised.exception.consumed)
        capture.assert_not_called()

    def test_continue_saved_account_login_rejects_a_stale_page_without_clicking(self):
        first_user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        first_login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        first_switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, first_window = install_tree(first_user, first_login, first_switch)
        first_window.geometry = (200, 100, 420, 600)
        second_user = FakeNode("Current UserBob", "label", (300, 145, 220, 30))
        second_login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        second_switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, second_window = install_tree(second_user, second_login, second_switch)
        second_window.geometry = (200, 100, 420, 600)

        with saved_account_screenshot() as screenshot:
            generation = expected_saved_auth_generation(
                first_window, screenshot["pixels"], screenshot["dimensions"],
            )
            with mock.patch.object(
                driver, "active_surface_root",
                side_effect=((first_window, (0,)), (second_window, (0,))),
            ):
                with mock.patch.object(
                    driver, "verified_window_identity", return_value=verified_wechat_identity(),
                ):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.dispatch({
                            "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                            "expected_auth_generation": generation,
                        })

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertFalse(raised.exception.consumed)
        self.assertEqual([], first_login.activated)
        self.assertEqual([], second_login.activated)

    def test_continue_saved_account_login_rejects_a_window_switch_without_clicking(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))

        with saved_account_screenshot() as screenshot:
            generation = expected_saved_auth_generation(
                window, screenshot["pixels"], screenshot["dimensions"],
            )
            with mock.patch.object(driver, "active_surface_root", return_value=scope):
                with mock.patch.object(
                    driver, "verified_window_identity",
                    side_effect=(verified_wechat_identity("a"), verified_wechat_identity("b")),
                ):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.dispatch({
                            "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                            "expected_auth_generation": generation,
                        })

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertFalse(raised.exception.consumed)
        self.assertEqual([], login.activated)
        self.assertEqual([], switch.activated)

    def test_third_capture_rejects_an_accessible_login_replacement(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        replacement = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))
        active_calls = 0

        def active_scope():
            nonlocal active_calls
            active_calls += 1
            if active_calls == 5:
                window.children[1] = replacement
            return scope

        with saved_account_screenshot() as screenshot:
            generation = expected_saved_auth_generation(
                window, screenshot["pixels"], screenshot["dimensions"],
            )
            with mock.patch.object(driver, "active_surface_root", side_effect=active_scope):
                with mock.patch.object(
                    driver, "verified_window_identity", return_value=verified_wechat_identity(),
                ):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.dispatch({
                            "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                            "expected_auth_generation": generation,
                        })

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], login.activated)
        self.assertEqual([], replacement.activated)

    def test_continue_saved_account_login_rejects_changed_rendered_account(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))
        first_pixels = fixture_pixels(value=0x22)
        changed_pixels = fixture_pixels(value=0x77)
        generation = expected_saved_auth_generation(window, first_pixels)

        with mock.patch.object(driver, "active_surface_root", return_value=scope):
            with mock.patch.object(
                driver, "verified_window_identity", return_value=verified_wechat_identity(),
            ):
                with mock.patch.object(driver, "capture_screenshot_png", return_value=b"png"):
                    with mock.patch.object(
                        driver, "screenshot_rgba",
                        side_effect=((first_pixels, (420, 600)), (changed_pixels, (420, 600))),
                    ):
                        with self.assertRaises(driver.ControlFailure) as raised:
                            driver.dispatch({
                                "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                                "expected_auth_generation": generation,
                            })

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertFalse(raised.exception.consumed)
        self.assertEqual([], login.activated)

    def test_continue_saved_account_login_requires_the_advertised_generation(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))

        with saved_account_screenshot():
            with mock.patch.object(driver, "active_surface_root", return_value=scope):
                with mock.patch.object(
                    driver, "verified_window_identity", return_value=verified_wechat_identity(),
                ):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.dispatch({
                            "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                            "expected_auth_generation": "f" * 64,
                        })

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], login.activated)

    def test_advertised_generation_is_bound_to_the_verified_window_identity(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))
        pixels = fixture_pixels()
        advertised = expected_saved_auth_generation(window, pixels, identity_digest="a" * 64)

        with saved_account_screenshot(pixels=pixels):
            with mock.patch.object(driver, "active_surface_root", return_value=scope):
                with mock.patch.object(
                    driver, "verified_window_identity",
                    return_value=verified_wechat_identity("b"),
                ):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.dispatch({
                            "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                            "expected_auth_generation": advertised,
                        })

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], login.activated)

    def test_final_action_interface_is_validated_and_used_as_one_object(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))
        query_count = 0
        executed = []

        class ChangingAction(FakeAction):
            def doAction(self, index):
                executed.append(self.names[index])
                return True

        def changing_query_action():
            nonlocal query_count
            query_count += 1
            name = "delete" if query_count == 13 else "click"
            return ChangingAction(login, (name,))

        login.queryAction = changing_query_action
        pixels = fixture_pixels()
        generation = expected_saved_auth_generation(window, pixels)
        with saved_account_screenshot(pixels=pixels):
            with mock.patch.object(driver, "active_surface_root", return_value=scope):
                with mock.patch.object(
                    driver, "verified_window_identity", return_value=verified_wechat_identity(),
                ):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.dispatch({
                            "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                            "expected_auth_generation": generation,
                        })

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual(13, query_count)
        self.assertEqual([], executed)

    def test_rejected_saved_account_click_is_reported_consumed_without_retry(self):
        user = FakeNode("Current UserAlice", "label", (300, 145, 220, 30))
        login = FakeNode(
            "Log In", "push button", (290, 380, 240, 50), actions=("click",),
        )
        switch = FakeNode(
            "Switch Account", "push button", (290, 455, 240, 40), actions=("click",),
        )

        class RejectingAction(FakeAction):
            def doAction(self, index):
                self.node.activated.append(index)
                return False

        login.queryAction = lambda: RejectingAction(login, ("click",))
        _root, window = install_tree(user, login, switch)
        window.geometry = (200, 100, 420, 600)
        scope = (window, (0,))
        emitted = []

        with saved_account_screenshot() as screenshot:
            generation = expected_saved_auth_generation(
                window, screenshot["pixels"], screenshot["dimensions"],
            )
            with mock.patch.object(driver, "active_surface_root", return_value=scope):
                with mock.patch.object(
                    driver, "verified_window_identity", return_value=verified_wechat_identity(),
                ):
                    with mock.patch.object(
                        driver, "read_request",
                        return_value={
                            "operation": driver.SAVED_ACCOUNT_AUTH_ACTION_ID,
                            "expected_auth_generation": generation,
                        },
                    ):
                        with mock.patch.object(driver, "emit", side_effect=emitted.append):
                            driver.main()

        self.assertEqual([0], login.activated)
        self.assertEqual([], switch.activated)
        self.assertEqual(1, len(emitted))
        self.assertFalse(emitted[0]["ok"])
        self.assertEqual("ACTION_OUTCOME_UNCERTAIN", emitted[0]["code"])
        self.assertTrue(emitted[0]["consumed"])

    def test_bound_surface_dispatch_does_not_require_main_window_online_evidence(self):
        with mock.patch.object(driver, "probe", side_effect=AssertionError("must not probe")):
            with mock.patch.object(driver, "surface_snapshot", return_value={"kind": "miniprogram"}):
                snapshot = driver.dispatch({
                    "operation": "snapshot_surface",
                    "expected_window_identity": "bound-window",
                })
            with mock.patch.object(driver, "act_surface", return_value={"kind": "miniprogram"}):
                acted = driver.dispatch({
                    "operation": "act_surface",
                    "expected_window_identity": "bound-window",
                    "locator": "bound-action",
                })

        self.assertEqual("miniprogram", snapshot["surface"]["kind"])
        self.assertEqual("miniprogram", acted["surface"]["kind"])

    def test_probe_and_conversation_activation_fail_when_window_owner_is_unverified(self):
        conversation = FakeNode(
            "Alice", "list item", (20, 100, 300, 50), actions=("click",),
        )
        conversation_list = FakeNode(
            "Chats", "list", (0, 60, 350, 700), children=(conversation,),
        )
        header = FakeNode("Alice", "label", (600, 40, 100, 30))
        install_tree(conversation_list, header)
        locator = driver.make_locator((0, 0, 0), "conversation", 0, node=conversation)
        failure = driver.ControlFailure("ACTION_STALE", "unbound")

        with mock.patch.object(driver, "verified_window_identity", side_effect=failure):
            self.assertEqual("DEGRADED", driver.probe()["state"])
            with self.assertRaises(driver.ControlFailure) as raised:
                driver.select_conversation("Alice", locator)

        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], conversation.activated)

    def test_named_miniprogram_duplicate_exact_results_are_ambiguous(self):
        first = FakeNode("校园瞄", "list item", (100, 180, 200, 40), actions=("click",))
        second = FakeNode("校园瞄", "list item", (100, 240, 200, 40), actions=("click",))
        section = FakeNode("小程序", "list", (50, 120, 400, 300), children=(first, second))
        _root, window = install_tree(section)
        frame = fake_surface_frame(window, process_kind="wechat")

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.unique_named_miniprogram_candidate("校园瞄", frame)

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)
        self.assertEqual([], first.activated)
        self.assertEqual([], second.activated)

    def test_named_result_without_miniprogram_context_is_rejected(self):
        contact = FakeNode("校园瞄", "list item", (100, 180, 200, 40), actions=("click",))
        contacts = FakeNode("联系人", "list", (50, 120, 400, 300), children=(contact,))
        _root, window = install_tree(contacts)
        frame = fake_surface_frame(window, process_kind="wechat")

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.unique_named_miniprogram_candidate("校园瞄", frame)

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        self.assertEqual([], contact.activated)

    def test_named_result_never_treats_a_destructive_single_action_as_open(self):
        result = FakeNode(
            "校园瞄", "list item", (100, 180, 200, 40), actions=("delete",),
        )
        section = FakeNode("小程序", "list", (50, 120, 400, 300), children=(result,))
        _root, window = install_tree(section)
        frame = fake_surface_frame(window, process_kind="wechat")

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.unique_named_miniprogram_candidate("校园瞄", frame)

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        self.assertEqual([], result.activated)

    def test_unrelated_miniprogram_marker_does_not_prove_contact_result(self):
        marker = FakeNode("小程序", "label", (20, 180, 60, 30))
        contact = FakeNode("校园瞄", "list item", (100, 180, 200, 40), actions=("click",))
        contacts = FakeNode("联系人", "list", (90, 120, 400, 300), children=(contact,))
        _root, window = install_tree(marker, contacts)
        frame = fake_surface_frame(window, process_kind="wechat")

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.unique_named_miniprogram_candidate("校园瞄", frame)

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        self.assertEqual([], contact.activated)

    def test_candidate_identity_includes_action_and_section_semantics(self):
        result = FakeNode("校园瞄", "text", (100, 180, 200, 40), actions=("click",))
        section = FakeNode("小程序", "list", (50, 120, 400, 300), children=(result,))
        _root, window = install_tree(section)
        first = driver.unique_named_miniprogram_candidate(
            "校园瞄", fake_surface_frame(window, process_kind="wechat"),
        )

        result.action_names = ["press"]
        second = driver.unique_named_miniprogram_candidate(
            "校园瞄", fake_surface_frame(window, process_kind="wechat"),
        )

        self.assertNotEqual(driver.candidate_identity(first), driver.candidate_identity(second))

    def test_named_candidate_uses_the_same_validated_action_interface(self):
        result = FakeNode("校园瞄", "list item", (100, 180, 200, 40), actions=("click",))
        section = FakeNode("小程序", "list", (50, 120, 400, 300), children=(result,))
        _root, window = install_tree(section)
        frame = fake_surface_frame(window, process_kind="wechat")
        candidate = driver.unique_named_miniprogram_candidate("校园瞄", frame)
        queries = 0
        executed = []

        class SwappingAction(FakeAction):
            def doAction(self, index):
                executed.append(self.names[index])
                return True

        def query_action():
            nonlocal queries
            queries += 1
            return SwappingAction(result, ("click" if queries == 1 else "delete",))

        result.queryAction = query_action
        driver.activate_named_candidate(candidate, frame)

        self.assertEqual(1, queries)
        self.assertEqual(["click"], executed)

    def test_named_candidate_waits_for_a_delayed_stable_result(self):
        _root, empty_window = install_tree()
        empty = fake_surface_frame(empty_window, process_kind="wechat")
        result = FakeNode("校园瞄", "list item", (100, 180, 200, 40), actions=("click",))
        section = FakeNode("小程序", "list", (50, 120, 400, 300), children=(result,))
        _root, result_window = install_tree(section)
        visible = fake_surface_frame(result_window, process_kind="wechat")

        with mock.patch.object(driver, "capture_surface_frame", side_effect=(empty, visible, visible)):
            with mock.patch.object(driver.time, "monotonic", return_value=0):
                with mock.patch.object(driver.time, "sleep"):
                    frame, candidate = driver.wait_for_named_candidate(
                        "校园瞄", visible["window_identity"],
                    )

        self.assertIs(visible, frame)
        self.assertEqual((0, 0, 0), candidate["path"])

    def test_opened_miniprogram_must_be_new_and_show_the_requested_name(self):
        _root, main_window = install_tree()
        initial = fake_surface_frame(main_window, process_kind="wechat")
        title = FakeNode("校园瞄", "label", (100, 20, 200, 40))
        _root, mini_window = install_tree(title)
        opened = fake_surface_frame(mini_window, process_kind="miniprogram")
        opened["window"]["xid"] = "0x9999"
        opened["window"]["pid"] = 99
        opened["window"]["pid_starttime"] = 999
        opened["window"].pop("digest")
        opened["window"]["digest"] = driver.semantic_digest({
            key: value for key, value in opened["window"].items() if key != "digest"
        })
        opened["window_identity"] = driver.encode_window_identity(opened["window"])

        with mock.patch.object(driver, "capture_surface_frame", side_effect=(initial, opened)):
            with mock.patch.object(driver.time, "monotonic", return_value=0):
                with mock.patch.object(driver.time, "sleep"):
                    result = driver.wait_for_opened_miniprogram(
                        "校园瞄", initial["window_identity"],
                        {driver.window_instance(initial["window"])},
                    )
        self.assertIs(opened, result)

        with mock.patch.object(driver, "capture_surface_frame", return_value=opened):
            with mock.patch.object(driver.time, "monotonic", return_value=0):
                with self.assertRaises(driver.ControlFailure) as raised:
                    driver.wait_for_opened_miniprogram(
                        "校园瞄", initial["window_identity"],
                        {driver.window_instance(opened["window"])},
                    )
        self.assertEqual("ACTION_STALE", raised.exception.code)

    def test_leftover_surface_focuses_unique_main_window_without_closing_it(self):
        _root, active_surface = install_tree()
        leftover = fake_surface_frame(active_surface, process_kind="miniprogram")
        _root, main_window = install_tree()
        main = fake_surface_frame(main_window, process_kind="wechat")

        with mock.patch.object(driver, "capture_surface_frame", side_effect=(leftover, main)):
            with mock.patch.object(driver, "x11_wechat_window_inventory", return_value={
                ("0x1234", 42, 123456, "miniprogram"),
                ("0x2222", 52, 223456, "wechat"),
            }):
                with mock.patch.object(driver, "focus_x11_window") as focus:
                    with mock.patch.object(
                        driver, "x11_active_window",
                        return_value=("0x2222", 52, ["wechat"], (0, 0, 1000, 800)),
                    ):
                        recovered = driver.main_wechat_frame_for_navigation()

        self.assertIs(main, recovered)
        focus.assert_called_once_with("0x2222")

    def test_leftover_surface_never_guesses_between_main_windows(self):
        _root, active_surface = install_tree()
        leftover = fake_surface_frame(active_surface, process_kind="miniprogram")
        with mock.patch.object(driver, "capture_surface_frame", return_value=leftover):
            with mock.patch.object(driver, "x11_wechat_window_inventory", return_value={
                ("0x2222", 52, 223456, "wechat"),
                ("0x3333", 62, 323456, "wechat"),
            }):
                with mock.patch.object(driver, "focus_x11_window") as focus:
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.main_wechat_frame_for_navigation()

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)
        focus.assert_not_called()

    def test_scroll_locator_survives_unrelated_surface_generation_changes(self):
        content = FakeNode("posts", "label", (100, 100, 400, 400))
        _root, window = install_tree(content)
        frame = fake_surface_frame(window)
        locator = driver.decode_locator(driver.make_locator(
            (0,), "scroll", node=window, source="viewport", direction="down",
            **frame_locator_fields(frame),
        ))
        with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
            validated, _scope = driver.validate_surface_locator(locator, expected_kind="scroll")
        self.assertIs(window, validated)

        content.name = "different posts"
        changed = fake_surface_frame(window)
        with mock.patch.object(driver, "capture_surface_frame", return_value=changed):
            validated, _scope = driver.validate_surface_locator(locator, expected_kind="scroll")
        self.assertIs(window, validated)

    def test_ocr_action_checks_text_bounds_and_screenshot(self):
        _root, window = install_tree()
        region = {"text": "View details", "bounds": (100, 150, 160, 30), "confidence": 0.9}
        frame = fake_surface_frame(window, ocr=(region,))
        signature = driver.ocr_region_signature(frame, region)
        locator = driver.decode_locator(driver.make_locator(
            (0,), "visual_activate", node=window, source="ocr",
            bounds_value=region["bounds"], text=region["text"],
            region_signature=signature, strict_frame=True, **frame_locator_fields(frame),
        ))
        with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
            validated, _scope = driver.validate_surface_locator(locator)
        self.assertIs(window, validated)

        changed = fake_surface_frame(
            window,
            ocr=({"text": "Different", "bounds": region["bounds"], "confidence": 0.9},),
            screenshot_sha256="b" * 64,
        )
        with self.assertRaises(driver.ControlFailure) as raised:
            with mock.patch.object(driver, "capture_surface_frame", return_value=changed):
                driver.validate_surface_locator(locator)
        self.assertEqual("ACTION_STALE", raised.exception.code)

    def test_atspi_action_survives_unrelated_pixel_change(self):
        button = FakeNode("Details", "push button", (100, 150, 160, 30), actions=("click",))
        _root, window = install_tree(button)
        frame = fake_surface_frame(window)
        locator = driver.decode_locator(driver.semantic_surface_actions(frame)[0]["locator"])
        changed = fake_surface_frame(window, screenshot_sha256="b" * 64)

        with mock.patch.object(driver, "capture_surface_frame", return_value=changed):
            validated, _scope = driver.validate_surface_locator(locator)

        self.assertIs(button, validated)

    def test_surface_locator_uses_the_same_validated_action_interface(self):
        button = FakeNode("Details", "push button", (100, 150, 160, 30), actions=("click",))
        _root, window = install_tree(button)
        frame = fake_surface_frame(window)
        locator = driver.decode_locator(driver.semantic_surface_actions(frame)[0]["locator"])
        queries = 0
        executed = []

        class SwappingAction(FakeAction):
            def doAction(self, index):
                executed.append(self.names[index])
                return True

        def query_action():
            nonlocal queries
            queries += 1
            return SwappingAction(button, ("click" if queries == 1 else "delete",))

        button.queryAction = query_action
        with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
            with mock.patch.object(driver, "require_safe_surface_frame"):
                with mock.patch.object(driver, "high_risk_surface_context", return_value=False):
                    with mock.patch.object(driver, "high_risk_visual_regions", return_value=[]):
                        node, scope, interface = driver.validate_surface_locator(
                            locator, return_action_interface=True,
                        )
                        driver.activate_locator(
                            node, locator, "", scope, validated_interface=interface,
                        )

        self.assertEqual(1, queries)
        self.assertEqual(["click"], executed)

    def test_unique_editable_search_action_validates_with_advertised_semantics(self):
        search = FakeNode(
            "Search", "entry", (100, 150, 300, 40),
            actions=("activate",), editable=True,
        )
        _root, window = install_tree(search)
        frame = fake_surface_frame(window)
        advertised = driver.semantic_surface_actions(frame)[0]
        locator = driver.decode_locator(advertised["locator"])

        self.assertEqual(("input", "low", "search_input", False), (
            advertised["kind"], advertised["risk"], advertised["effect"],
            locator["strict_frame"],
        ))
        with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
            validated, _scope = driver.validate_surface_locator(
                locator, expected_kind="input",
            )

        self.assertIs(search, validated)

    def test_atspi_action_survives_unrelated_semantic_change(self):
        button = FakeNode("Details", "push button", (100, 150, 160, 30), actions=("click",))
        timer = FakeNode("00:10", "label", (700, 30, 80, 30))
        _root, window = install_tree(button, timer)
        frame = fake_surface_frame(window)
        advertised = driver.semantic_surface_actions(frame)[0]
        locator = driver.decode_locator(advertised["locator"])
        timer.name = "00:09"
        changed = fake_surface_frame(window)
        current = driver.semantic_surface_actions(changed)[0]

        self.assertNotEqual(frame["generation"], changed["generation"])
        self.assertEqual(advertised["target_id"], current["target_id"])
        self.assertEqual(advertised["id"], current["id"])
        with mock.patch.object(driver, "capture_surface_frame", return_value=changed):
            validated, _scope = driver.validate_surface_locator(locator)
        self.assertIs(button, validated)

    def test_sensitive_action_is_bound_to_the_exact_observed_frame(self):
        total = FakeNode("Total: $10", "label", (600, 600, 180, 30))
        submit = FakeNode("Submit", "push button", (700, 700, 120, 40), actions=("click",))
        _root, window = install_tree(total, submit)
        first_frame = fake_surface_frame(window)
        first = driver.semantic_surface_actions(first_frame)[0]
        locator = driver.decode_locator(first["locator"])

        self.assertEqual(("medium", "external_write", True), (
            first["risk"], first["effect"], locator["strict_frame"],
        ))
        total.name = "Total: $1000"
        changed_frame = fake_surface_frame(window, screenshot_sha256="b" * 64)
        changed = driver.semantic_surface_actions(changed_frame)[0]

        self.assertNotEqual(first["id"], changed["id"])
        self.assertEqual(first["replay_id"], changed["replay_id"])
        with self.assertRaises(driver.ControlFailure) as raised:
            with mock.patch.object(driver, "capture_surface_frame", return_value=changed_frame):
                driver.validate_surface_locator(locator)
        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], submit.activated)

    def test_sensitive_action_ignores_volatile_png_encoding_metadata(self):
        submit = FakeNode("Submit", "push button", (700, 700, 120, 40), actions=("click",))
        _root, window = install_tree(submit)
        first_frame = fake_surface_frame(window, screenshot_sha256="a" * 64)
        locator = driver.decode_locator(
            driver.semantic_surface_actions(first_frame)[0]["locator"],
        )
        reencoded = fake_surface_frame(window, screenshot_sha256="b" * 64)
        reencoded["screenshot_rgba"] = first_frame["screenshot_rgba"]
        reencoded["rendered_frame_sha256"] = driver.rendered_frame_digest(
            reencoded["screenshot_rgba"], reencoded["screenshot_dimensions"],
        )

        self.assertNotEqual(first_frame["screenshot_sha256"], reencoded["screenshot_sha256"])
        self.assertEqual(
            first_frame["rendered_frame_sha256"], reencoded["rendered_frame_sha256"],
        )
        with mock.patch.object(driver, "capture_surface_frame", return_value=reencoded):
            validated, _scope = driver.validate_surface_locator(locator)
        self.assertIs(submit, validated)

    def test_locator_rejects_non_boolean_strict_frame_policy(self):
        button = FakeNode("Submit", "push button", (700, 700, 120, 40), actions=("click",))
        _root, window = install_tree(button)
        frame = fake_surface_frame(window)
        original = driver.decode_locator(driver.semantic_surface_actions(frame)[0]["locator"])
        for name, mutate in (
            ("non-boolean", lambda value: value.__setitem__("strict_frame", "true")),
            ("missing", lambda value: value.pop("strict_frame")),
            ("false downgrade", lambda value: value.__setitem__("strict_frame", False)),
            ("old version", lambda value: value.__setitem__("version", 3)),
        ):
            with self.subTest(name=name):
                locator = dict(original)
                mutate(locator)
                with self.assertRaises(driver.ControlFailure) as raised:
                    with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
                        driver.validate_surface_locator(locator)
                self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], button.activated)

    def test_capture_survives_read_only_timer_change_during_ocr(self):
        button = FakeNode("Details", "push button", (100, 150, 160, 30), actions=("click",))
        timer = FakeNode("00:10", "label", (700, 30, 80, 30))
        _root, window = install_tree(button, timer)
        fixture = fake_surface_frame(window)

        def run_ocr(_screenshot):
            timer.name = "00:09"
            return []

        with mock.patch.object(driver, "active_surface_root", return_value=fixture["scope"]):
            with mock.patch.object(driver, "verified_window_identity", return_value=fixture["window"]):
                with mock.patch.object(driver, "capture_screenshot_png", return_value=b"png"):
                    with mock.patch.object(
                        driver, "screenshot_rgba",
                        return_value=(fixture["screenshot_rgba"], fixture["screenshot_dimensions"]),
                    ):
                        with mock.patch.object(driver, "tesseract_regions", side_effect=run_ocr):
                            frame = driver.capture_surface_frame(fixture["window_identity"])

        self.assertEqual("00:09", timer.name)
        self.assertEqual(64, len(frame["generation"]))

    def test_ocr_action_rejects_local_pixel_change_with_same_semantics(self):
        _root, window = install_tree()
        region = {"text": "Dormitory", "bounds": (100, 150, 160, 30), "confidence": 0.9}
        frame = fake_surface_frame(window, ocr=(region,))
        signature = driver.ocr_region_signature(frame, region)
        locator = driver.decode_locator(driver.make_locator(
            (0,), "visual_activate", node=window, source="ocr",
            bounds_value=region["bounds"], text=region["text"],
            region_signature=signature, strict_frame=True, **frame_locator_fields(frame),
        ))
        changed = fake_surface_frame(window, ocr=(region,))
        pixels = bytearray(changed["screenshot_rgba"])
        offset = (160 * changed["screenshot_dimensions"][0] + 110) * 4
        pixels[offset] ^= 0xFF
        changed["screenshot_rgba"] = bytes(pixels)

        with self.assertRaises(driver.ControlFailure) as raised:
            with mock.patch.object(driver, "capture_surface_frame", return_value=changed):
                driver.validate_surface_locator(locator)

        self.assertEqual("ACTION_STALE", raised.exception.code)

    def test_risky_ocr_action_is_visible_but_permanently_disabled(self):
        _root, window = install_tree()
        frame = fake_surface_frame(
            window,
            ocr=({"text": "Payment", "bounds": (100, 100, 120, 30), "confidence": 0.9},),
        )

        surface = driver.surface_snapshot(frame)
        payment = next(item for item in surface["actions"] if item["label"] == "Payment")
        self.assertEqual(("high", "high_risk", True), (
            payment["risk"], payment["effect"], payment["disabled"],
        ))

    def test_unproven_ocr_clicks_and_permission_purchase_labels_are_disabled(self):
        for label in ("Mystery", "Buy now", "Allow", "Grant access"):
            with self.subTest(label=label):
                _root, window = install_tree()
                frame = fake_surface_frame(
                    window,
                    ocr=({
                        "text": label, "bounds": (100, 100, 180, 36), "confidence": 0.9,
                    },),
                )
                action = next(
                    item for item in driver.surface_snapshot(frame)["actions"]
                    if item["label"] == label
                )
                self.assertEqual(("high", "high_risk", True), (
                    action["risk"], action["effect"], action["disabled"],
                ))
                locator = driver.decode_locator(action["locator"])
                with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.validate_surface_locator(locator)
                self.assertEqual("USER_ACTION_REQUIRED", raised.exception.code)

    def test_proven_ocr_search_navigation_and_viewport_scroll_remain_available(self):
        _root, window = install_tree()
        frame = fake_surface_frame(window, ocr=(
            {"text": "Search posts", "bounds": (100, 80, 240, 40), "confidence": 0.9},
            {"text": "View details", "bounds": (100, 180, 240, 40), "confidence": 0.9},
            {"text": "Back", "bounds": (20, 20, 80, 30), "confidence": 0.9},
        ))
        actions = {item["label"]: item for item in driver.surface_snapshot(frame)["actions"]}
        self.assertEqual(("input", "low", "search_input", False), (
            actions["Search posts"]["kind"], actions["Search posts"]["risk"],
            actions["Search posts"]["effect"], actions["Search posts"]["disabled"],
        ))
        self.assertEqual(("visual_activate", "low", "navigate", False), (
            actions["View details"]["kind"], actions["View details"]["risk"],
            actions["View details"]["effect"], actions["View details"]["disabled"],
        ))
        self.assertEqual(("low", "navigate", False), (
            actions["Back"]["risk"], actions["Back"]["effect"], actions["Back"]["disabled"],
        ))
        self.assertEqual(("low", "observe", False), (
            actions["Scroll viewport down"]["risk"],
            actions["Scroll viewport down"]["effect"],
            actions["Scroll viewport down"]["disabled"],
        ))

    def test_spaced_cjk_ocr_cannot_bypass_high_risk_markers(self):
        _root, window = install_tree()
        frame = fake_surface_frame(
            window,
            ocr=({"text": "支 付", "bounds": (100, 100, 120, 30), "confidence": 0.9},),
        )
        surface = driver.surface_snapshot(frame)
        payment = next(item for item in surface["actions"] if item["label"] == "支 付")
        self.assertTrue(payment["disabled"])
        self.assertEqual("校园瞄", driver.normalized_exact("校 园 瞄"))

    def test_research_text_is_not_misclassified_as_visual_search_input(self):
        self.assertEqual("visual_activate", driver.ocr_action_kind("Research notes"))
        self.assertEqual("input", driver.ocr_action_kind("Search posts"))
        self.assertEqual("input", driver.ocr_action_kind("搜索帖子"))

    def test_read_only_post_can_mention_orders_or_reports(self):
        post = FakeNode("订单问题应该如何举报", "paragraph", (100, 100, 400, 80))
        _root, window = install_tree(post)
        surface = driver.surface_snapshot(fake_surface_frame(window))
        self.assertIn("订单问题应该如何举报", surface["semantic_text"])

    def test_merged_canvas_security_prompt_with_continue_is_blocked(self):
        _root, window = install_tree()
        frame = fake_surface_frame(window, ocr=(
            {"text": "安全验证 请完成验证", "bounds": (100, 80, 300, 40), "confidence": 0.9},
            {"text": "Continue", "bounds": (700, 650, 120, 40), "confidence": 0.9},
        ))

        self.assertTrue(driver.security_challenge_context(frame))
        with self.assertRaises(driver.ControlFailure) as raised:
            driver.require_safe_surface_frame(frame)
        self.assertEqual("USER_ACTION_REQUIRED", raised.exception.code)

    def test_security_words_in_an_ordinary_canvas_post_stay_readable(self):
        _root, window = install_tree()
        frame = fake_surface_frame(window, ocr=(
            {
                "text": "关于人脸识别和风险提示的经验分享",
                "bounds": (100, 120, 500, 80), "confidence": 0.9,
            },
            {"text": "风险提示", "bounds": (100, 260, 160, 30), "confidence": 0.9},
        ))

        self.assertFalse(driver.security_challenge_context(frame))
        surface = driver.surface_snapshot(frame)
        self.assertIn("关于人脸识别和风险提示的经验分享", surface["semantic_text"])

    def test_payment_context_disables_continue_but_keeps_back_and_scroll(self):
        title = FakeNode("Payment", "label", (100, 30, 200, 30))
        proceed = FakeNode("Continue", "push button", (700, 700, 120, 40), actions=("click",))
        back = FakeNode("Back", "push button", (20, 20, 80, 30), actions=("click",))
        disguised = FakeNode("Back and submit", "push button", (500, 700, 160, 40), actions=("click",))
        _root, window = install_tree(title, proceed, back, disguised)
        surface = driver.surface_snapshot(fake_surface_frame(window))
        actions = {item["label"]: item for item in surface["actions"]}

        self.assertTrue(actions["Continue"]["disabled"])
        self.assertEqual("high_risk", actions["Continue"]["effect"])
        self.assertFalse(actions["Back"]["disabled"])
        self.assertTrue(actions["Back and submit"]["disabled"])
        self.assertFalse(actions["Scroll viewport down"]["disabled"])

    def test_asset_token_is_stable_for_same_frame(self):
        image = FakeNode("post image", "image", (100, 100, 300, 200))
        _root, window = install_tree(image)
        frame = fake_surface_frame(window)

        _elements, first = driver.atspi_elements_and_assets(frame)
        _elements, second = driver.atspi_elements_and_assets(frame)

        self.assertEqual(first[0]["token"], second[0]["token"])
        self.assertEqual([100, 100, 300, 200], first[0]["bounds"])

    def test_canvas_surface_always_has_a_rendered_viewport_asset(self):
        _root, window = install_tree()
        frame = fake_surface_frame(window, ocr=(
            {"text": "Canvas-only post", "bounds": (100, 100, 300, 60), "confidence": 0.9},
        ))

        first = driver.surface_snapshot(frame)
        second = driver.surface_snapshot(frame)
        viewport = next(item for item in first["assets"] if item["kind"] == "rendered_viewport")
        repeated = next(item for item in second["assets"] if item["kind"] == "rendered_viewport")

        self.assertEqual([0, 0, 1000, 800], viewport["bounds"])
        self.assertEqual("rendered", viewport["source"])
        self.assertEqual("Rendered viewport", viewport["label"])
        self.assertEqual(viewport["token"], repeated["token"])

    def test_action_effects_do_not_default_unknown_or_writes_to_low_risk(self):
        self.assertEqual(
            ("activate", "medium", "external_write"),
            driver.classify_action_effect("收藏", "push button", "click"),
        )
        self.assertEqual(
            ("activate", "medium", "unknown"),
            driver.classify_action_effect("Mystery", "push button", "click"),
        )
        self.assertEqual(
            ("activate", "low", "navigate"),
            driver.classify_action_effect("Details", "link", "jump"),
        )

    def test_destructive_actions_are_permanently_disabled(self):
        for label in ("Delete", "Remove", "删除", "清空数据", "Erase data"):
            with self.subTest(label=label):
                button = FakeNode(label, "push button", (700, 700, 120, 40), actions=("click",))
                _root, window = install_tree(button)
                frame = fake_surface_frame(window)
                action = driver.semantic_surface_actions(frame)[0]
                locator = driver.decode_locator(action["locator"])

                self.assertEqual(("high", "high_risk", True), (
                    action["risk"], action["effect"], action["disabled"],
                ))
                with self.assertRaises(driver.ControlFailure) as raised:
                    with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
                        driver.validate_surface_locator(locator)
                self.assertEqual("USER_ACTION_REQUIRED", raised.exception.code)
                self.assertEqual([], button.activated)

    def test_invisible_format_characters_cannot_split_safety_markers(self):
        for label in (
            "D\u200belete", "删\u200b除", "P\u200bay", "支\u200b付", "Auth\u200borize",
            "D\u034felete", "D\ufe0felete", "删\u034f除", "P\ufe0fay",
        ):
            with self.subTest(label=label):
                self.assertEqual(
                    ("activate", "high", "high_risk"),
                    driver.classify_action_effect(label, "push button", "click"),
                )

    def test_high_risk_ocr_disables_overlapping_accessibility_action(self):
        disguised = FakeNode(
            "Proceed", "push button", (700, 700, 120, 40), actions=("click",),
        )
        _root, window = install_tree(disguised)
        frame = fake_surface_frame(
            window,
            ocr=({"text": "Delete", "bounds": (710, 705, 90, 30), "confidence": 0.95},),
        )
        surface = driver.surface_snapshot(frame)
        accessibility = next(
            action for action in surface["actions"] if action["source"] == "atspi"
        )

        self.assertEqual(("high", "high_risk", True), (
            accessibility["risk"], accessibility["effect"], accessibility["disabled"],
        ))
        locator = driver.decode_locator(accessibility["locator"])
        with self.assertRaises(driver.ControlFailure) as raised:
            with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
                driver.validate_surface_locator(locator)
        self.assertEqual("USER_ACTION_REQUIRED", raised.exception.code)
        self.assertEqual([], disguised.activated)

    def test_clickable_text_is_not_an_input_and_description_cannot_bypass_risk(self):
        post = FakeNode("宿舍帖子", "text", actions=("click",), editable=False)
        _root, window = install_tree(post)
        action = driver.semantic_surface_actions(fake_surface_frame(window))[0]
        self.assertEqual(("activate", "medium", "unknown"), (
            action["kind"], action["risk"], action["effect"],
        ))
        self.assertEqual(
            ("activate", "medium", "external_write"),
            driver.classify_action_effect(
                "button", "link", "jump", description="收藏", editable=False,
            ),
        )
        self.assertEqual(
            ("activate", "high", "high_risk"),
            driver.classify_action_effect(
                "button", "link", "jump", description="授权", editable=False,
            ),
        )
        self.assertEqual(
            ("activate", "medium", "unknown"),
            driver.classify_action_effect("Feedback", "push button", "click"),
        )
        self.assertEqual(
            ("activate", "medium", "external_write"),
            driver.classify_action_effect("保存并关闭", "push button", "click"),
        )
        self.assertEqual(
            ("activate", "medium", "external_write"),
            driver.classify_action_effect("Back and submit", "push button", "click"),
        )
        self.assertEqual(
            ("activate", "high", "high_risk"),
            driver.classify_action_effect("关闭账号", "push button", "click"),
        )

    def test_target_links_and_action_ids_are_stable_across_locator_issue_time(self):
        first = FakeNode("帖子", "text", (100, 100, 100, 30), actions=("click",))
        second = FakeNode("帖子", "text", (100, 180, 100, 30), actions=("click",))
        _root, window = install_tree(first, second)
        frame = fake_surface_frame(window)
        with mock.patch.object(driver.time, "time", return_value=100):
            elements, _assets = driver.atspi_elements_and_assets(frame)
            first_actions = driver.semantic_surface_actions(frame)
        with mock.patch.object(driver.time, "time", return_value=102):
            second_actions = driver.semantic_surface_actions(frame)

        self.assertEqual(
            [item["id"] for item in first_actions],
            [item["id"] for item in second_actions],
        )
        self.assertNotEqual(first_actions[0]["locator"], second_actions[0]["locator"])
        post_elements = [item for item in elements if item["label"] == "帖子"]
        self.assertEqual(post_elements[0]["target_id"], first_actions[0]["target_id"])
        self.assertEqual(post_elements[1]["target_id"], first_actions[1]["target_id"])
        self.assertNotEqual(post_elements[0]["target_id"], post_elements[1]["target_id"])

    def test_same_back_target_gets_a_new_action_after_page_context_changes(self):
        back = FakeNode("Back", "push button", (20, 20, 80, 30), actions=("click",))
        page = FakeNode("Page A", "panel", (0, 0, 1000, 800), children=(back,))
        _root, window = install_tree(page)
        first = driver.semantic_surface_actions(fake_surface_frame(window))[0]
        page.name = "Page B"
        second = driver.semantic_surface_actions(fake_surface_frame(window))[0]

        self.assertEqual(first["target_id"], second["target_id"])
        self.assertEqual(first["replay_id"], second["replay_id"])
        self.assertNotEqual(first["id"], second["id"])

    def test_unknown_editor_cannot_change_its_own_replay_identity(self):
        editor = FakeNode(
            "Comment", "text", (100, 150, 300, 45), editable=True, text="draft",
        )
        _root, window = install_tree(editor)
        first = driver.semantic_surface_actions(fake_surface_frame(window))[0]
        editor.text = "replaced"
        second = driver.semantic_surface_actions(fake_surface_frame(window))[0]

        self.assertEqual("unknown", first["effect"])
        self.assertEqual(first["target_id"], second["target_id"])
        self.assertEqual(first["replay_id"], second["replay_id"])
        self.assertNotEqual(first["id"], second["id"])

    def test_canvas_navigation_gets_new_id_but_stable_replay_identity(self):
        _root, window = install_tree()
        region = {"text": "Back", "bounds": (20, 20, 80, 30), "confidence": 0.9}
        first_frame = fake_surface_frame(window, ocr=(region,))
        second_frame = fake_surface_frame(window, ocr=(region,), screenshot_sha256="b" * 64)
        second_frame["screenshot_rgba"] = first_frame["screenshot_rgba"]
        first = driver.ocr_elements_and_actions(first_frame, [], [])[1][0]
        second = driver.ocr_elements_and_actions(second_frame, [], [])[1][0]

        self.assertEqual(first["replay_id"], second["replay_id"])
        self.assertNotEqual(first["id"], second["id"])

    def test_visual_replay_identity_survives_local_pixel_changes(self):
        _root, window = install_tree()
        region = {"text": "Mystery", "bounds": (20, 20, 80, 30), "confidence": 0.9}
        first_frame = fake_surface_frame(window, ocr=(region,))
        second_frame = fake_surface_frame(window, ocr=(region,), screenshot_sha256="b" * 64)
        first = driver.ocr_elements_and_actions(first_frame, [], [])[1][0]
        second = driver.ocr_elements_and_actions(second_frame, [], [])[1][0]

        self.assertNotEqual(first["target_id"], second["target_id"])
        self.assertNotEqual(first["id"], second["id"])
        self.assertEqual(first["replay_id"], second["replay_id"])

    def test_mutation_replay_identity_survives_reflow_and_new_x11_window(self):
        for label, expected_effect in (("Submit", "external_write"), ("Mystery", "unknown")):
            with self.subTest(label=label):
                first_button = FakeNode(
                    label, "push button", (100, 140, 180, 40), actions=("click",),
                )
                _first_root, first_window = install_tree(first_button)
                first_frame = fake_surface_frame(first_window)
                first = driver.semantic_surface_actions(first_frame)[0]

                second_button = FakeNode(
                    label, "push button", (620, 690, 180, 40), actions=("click",),
                )
                wrapper = FakeNode("", "panel", (0, 0, 1000, 800), children=(second_button,))
                _second_root, second_window = install_tree(wrapper)
                second_frame = fake_surface_frame(second_window)
                identity = dict(second_frame["window"])
                identity["xid"] = "0x9876"
                identity.pop("digest", None)
                identity["digest"] = driver.semantic_digest(identity)
                second_frame["window"] = identity
                second_frame["window_identity"] = driver.encode_window_identity(identity)
                second = driver.semantic_surface_actions(second_frame)[0]

                self.assertEqual(expected_effect, first["effect"])
                self.assertEqual(first["replay_id"], second["replay_id"])
                self.assertNotEqual(first["target_id"], second["target_id"])
                self.assertNotEqual(first["id"], second["id"])

    def test_visual_mutation_replay_identity_ignores_bounds_and_window(self):
        first_region = {"text": "Publish", "bounds": (100, 140, 180, 40), "confidence": 0.9}
        _first_root, first_window = install_tree()
        first_frame = fake_surface_frame(first_window, ocr=(first_region,))
        first = driver.ocr_elements_and_actions(first_frame, [], [])[1][0]

        second_region = {"text": "Publish", "bounds": (620, 690, 180, 40), "confidence": 0.9}
        _second_root, second_window = install_tree()
        second_frame = fake_surface_frame(second_window, ocr=(second_region,))
        identity = dict(second_frame["window"])
        identity["xid"] = "0x9876"
        identity.pop("digest", None)
        identity["digest"] = driver.semantic_digest(identity)
        second_frame["window"] = identity
        second_frame["window_identity"] = driver.encode_window_identity(identity)
        second = driver.ocr_elements_and_actions(second_frame, [], [])[1][0]

        self.assertEqual("external_write", first["effect"])
        self.assertEqual(first["replay_id"], second["replay_id"])
        self.assertNotEqual(first["target_id"], second["target_id"])
        self.assertNotEqual(first["id"], second["id"])

    def test_read_only_atspi_text_keeps_visual_action_fallback(self):
        label = FakeNode("宿舍", "text", (100, 150, 160, 30))
        _root, window = install_tree(label)
        region = {"text": "宿舍", "bounds": (100, 150, 160, 30), "confidence": 0.9}
        frame = fake_surface_frame(window, ocr=(region,))
        elements, _assets = driver.atspi_elements_and_assets(frame)
        semantic_actions = driver.semantic_surface_actions(frame)
        ocr_elements, ocr_actions = driver.ocr_elements_and_actions(
            frame, elements, semantic_actions,
        )

        self.assertEqual([], semantic_actions)
        self.assertEqual([], ocr_elements)
        self.assertEqual(1, len(ocr_actions))
        label_element = next(item for item in elements if item["label"] == "宿舍")
        self.assertEqual(label_element["target_id"], ocr_actions[0]["target_id"])

    def test_visual_input_requires_the_clicked_editor_to_be_focused(self):
        editor = FakeNode("搜索", "entry", (100, 150, 160, 30), editable=True)
        _root, window = install_tree(editor)
        frame = fake_surface_frame(window)
        locator = driver.decode_locator(driver.make_locator(
            (0,), "input", node=window, source="ocr",
            bounds_value=editor.geometry, text="搜索", region_signature="fixture",
            strict_frame=True, **frame_locator_fields(frame),
        ))
        with mock.patch.object(driver, "revalidate_visual_region", return_value=frame):
            with mock.patch.object(driver, "visual_pointer_action"):
                with mock.patch.object(driver, "active_surface_root", return_value=frame["scope"]):
                    with mock.patch.object(driver, "verified_window_identity", return_value=frame["window"]):
                        with self.assertRaises(driver.ControlFailure) as raised:
                            driver.activate_locator(window, locator, "宿舍", frame["scope"])
        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)
        self.assertEqual("", editor.text)

        editor.states.add("focused")
        with mock.patch.object(driver, "revalidate_visual_region", return_value=frame):
            with mock.patch.object(driver, "visual_pointer_action"):
                with mock.patch.object(driver, "active_surface_root", return_value=frame["scope"]):
                    with mock.patch.object(driver, "verified_window_identity", return_value=frame["window"]):
                        driver.activate_locator(window, locator, "宿舍", frame["scope"])
        self.assertEqual("宿舍", editor.text)

    def test_visual_target_change_is_rejected_before_any_pointer_event(self):
        _root, window = install_tree()
        frame = fake_surface_frame(window)
        locator = driver.decode_locator(driver.make_locator(
            (0,), "visual_activate", node=window, source="ocr",
            bounds_value=(100, 150, 160, 30), text="View details", region_signature="old",
            strict_frame=True, **frame_locator_fields(frame),
        ))
        failure = driver.ControlFailure("ACTION_STALE", "changed")
        with mock.patch.object(driver, "revalidate_visual_region", side_effect=failure):
            with mock.patch.object(driver, "visual_pointer_action") as pointer:
                with self.assertRaises(driver.ControlFailure) as raised:
                    driver.activate_locator(window, locator, "", frame["scope"])
        self.assertEqual("ACTION_STALE", raised.exception.code)
        pointer.assert_not_called()

    def test_visual_activation_runs_real_raw_revalidation_path(self):
        _root, window = install_tree()
        region = {"text": "View details", "bounds": (100, 150, 160, 30), "confidence": 0.9}
        frame = fake_surface_frame(window, ocr=(region,))
        signature = driver.ocr_region_signature(frame, region)
        locator = driver.decode_locator(driver.make_locator(
            (0,), "visual_activate", node=window, source="ocr",
            bounds_value=region["bounds"], text=region["text"],
            region_signature=signature, strict_frame=True, **frame_locator_fields(frame),
        ))
        with mock.patch.object(driver, "active_surface_root", return_value=frame["scope"]):
            with mock.patch.object(driver, "verified_window_identity", return_value=frame["window"]):
                with mock.patch.object(driver, "capture_screenshot_png", return_value=b"png"):
                    with mock.patch.object(
                        driver, "screenshot_rgba",
                        return_value=(frame["screenshot_rgba"], frame["screenshot_dimensions"]),
                    ):
                        with mock.patch.object(driver, "tesseract_regions", return_value=[region]):
                            with mock.patch.object(driver, "visual_pointer_action") as pointer:
                                driver.activate_locator(window, locator, "", frame["scope"])
        pointer.assert_called_once_with(
            region["bounds"], 1,
            expected_window_identity=frame["window_identity"],
            expected_rendered_frame_sha256=frame["rendered_frame_sha256"],
        )

    def test_final_visual_revalidation_checks_full_frame_and_current_ocr_risk(self):
        _root, window = install_tree()
        region = {"text": "View details", "bounds": (100, 150, 160, 30), "confidence": 0.9}
        observed = fake_surface_frame(window, ocr=(region,))
        signature = driver.ocr_region_signature(observed, region)

        changed = fake_surface_frame(window, ocr=(region,), screenshot_sha256="b" * 64)
        with mock.patch.object(driver, "capture_surface_frame", return_value=changed):
            with self.assertRaises(driver.ControlFailure) as raised:
                driver.revalidate_visual_region(
                    observed["window_identity"], region, signature,
                    observed["rendered_frame_sha256"], observed["generation"],
                )
        self.assertEqual("ACTION_STALE", raised.exception.code)

        risky_region = {"text": "View details", "bounds": (100, 400, 160, 30), "confidence": 0.9}
        risky = fake_surface_frame(window, ocr=(
            {"text": "Payment", "bounds": (100, 60, 180, 35), "confidence": 0.9},
            risky_region,
            {"text": "Pay now", "bounds": (700, 680, 140, 40), "confidence": 0.9},
        ))
        risky_signature = driver.ocr_region_signature(risky, risky_region)
        with mock.patch.object(driver, "capture_surface_frame", return_value=risky):
            with self.assertRaises(driver.ControlFailure) as raised:
                driver.revalidate_visual_region(
                    risky["window_identity"], risky_region, risky_signature,
                    risky["rendered_frame_sha256"], risky["generation"],
                )
        self.assertEqual("USER_ACTION_REQUIRED", raised.exception.code)

    def test_x11_bound_window_requires_exact_active_xid_geometry_and_owner(self):
        _root, window = install_tree()
        identity = fake_surface_frame(window)["window"]

        class Property:
            def __init__(self, value):
                self.value = [value]

        class Window:
            def get_geometry(self):
                return types.SimpleNamespace(width=1000, height=800)

            def get_full_property(self, _atom, _kind):
                return Property(42)

        class Root:
            def __init__(self, active):
                self.active = active
                self.translated = (0, 0)

            def get_full_property(self, _atom, _kind):
                return Property(self.active)

            def translate_coords(self, source, x, y):
                self.translate_call = (source, x, y)
                return types.SimpleNamespace(x=self.translated[0], y=self.translated[1])

        class Connection:
            def __init__(self, active):
                self.root = Root(active)
                self.window = Window()

            def screen(self):
                return types.SimpleNamespace(root=self.root)

            def intern_atom(self, name, only_if_exists=True):
                return {"_NET_ACTIVE_WINDOW": 1, "_NET_WM_PID": 2}[name]

            def create_resource_object(self, kind, xid):
                self.assertion = (kind, xid)
                return self.window

        fake_x = types.SimpleNamespace(AnyPropertyType=0)
        with mock.patch.object(
            driver, "process_record",
            return_value={"starttime": identity["pid_starttime"]},
        ):
            connection = Connection(int(identity["xid"], 16))
            _bound_root, bound_window, geometry = driver.x11_bound_window(
                connection, identity, fake_x,
            )
            self.assertIsInstance(bound_window, Window)
            self.assertEqual((0, 0, 1000, 800), geometry)
            self.assertEqual((bound_window, 0, 0), connection.root.translate_call)

            with self.assertRaises(driver.ControlFailure) as raised:
                driver.x11_bound_window(Connection(0x9999), identity, fake_x)
            self.assertEqual("ACTION_STALE", raised.exception.code)

            moved = Connection(int(identity["xid"], 16))
            moved.root.translated = (1, 0)
            with self.assertRaises(driver.ControlFailure) as raised:
                driver.x11_bound_window(moved, identity, fake_x)
            self.assertEqual("ACTION_STALE", raised.exception.code)

    def test_visual_x11_grab_covers_full_digest_check_and_pointer_injection(self):
        _root, window = install_tree()
        frame = fake_surface_frame(window)
        events = []

        class Connection:
            def grab_server(self):
                events.append("grab")

            def ungrab_server(self):
                events.append("ungrab")

            def sync(self):
                events.append("sync")

            def close(self):
                events.append("close")

        connection = Connection()
        fake_x = types.SimpleNamespace(
            AnyPropertyType=0, ZPixmap=2, TrueColor=4, LSBFirst=0, MSBFirst=1,
            MotionNotify="motion", ButtonPress="press", ButtonRelease="release",
        )
        display_module = types.ModuleType("Xlib.display")
        display_module.Display = lambda: connection
        xtest_module = types.ModuleType("Xlib.ext.xtest")
        xtest_module.fake_input = lambda _connection, event, *_args, **_kwargs: events.append(event)
        ext_module = types.ModuleType("Xlib.ext")
        ext_module.xtest = xtest_module
        xlib_module = types.ModuleType("Xlib")
        xlib_module.X = fake_x
        xlib_module.display = display_module

        def bind(_connection, _identity, _x_module):
            events.append("bound")
            return object(), object(), tuple(frame["window"]["geometry"])

        def pixels(_connection, _window, _geometry, _x_module):
            events.append("frame")
            return frame["screenshot_rgba"], frame["screenshot_dimensions"]

        modules = {
            "Xlib": xlib_module, "Xlib.display": display_module,
            "Xlib.ext": ext_module, "Xlib.ext.xtest": xtest_module,
        }
        with mock.patch.dict(sys.modules, modules):
            with mock.patch.object(driver, "x11_bound_window", side_effect=bind):
                with mock.patch.object(driver, "x11_window_rgba", side_effect=pixels):
                    driver.visual_pointer_action(
                        (100, 150, 160, 30), 1,
                        expected_window_identity=frame["window_identity"],
                        expected_rendered_frame_sha256=frame["rendered_frame_sha256"],
                    )
        self.assertEqual(
            ["grab", "bound", "frame", "motion", "press", "release", "sync", "ungrab", "sync", "close"],
            events,
        )

        events.clear()
        different_pixels = bytes((0, 0, 0, 255)) * (1000 * 800)
        with mock.patch.dict(sys.modules, modules):
            with mock.patch.object(driver, "x11_bound_window", side_effect=bind):
                with mock.patch.object(
                    driver, "x11_window_rgba",
                    side_effect=lambda *_args: (
                        events.append("frame") or different_pixels,
                        frame["screenshot_dimensions"],
                    ),
                ):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.visual_pointer_action(
                            (100, 150, 160, 30), 1,
                            expected_window_identity=frame["window_identity"],
                            expected_rendered_frame_sha256=frame["rendered_frame_sha256"],
                        )
        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual(["grab", "bound", "frame", "ungrab", "sync", "close"], events)

    def test_main_wechat_window_is_never_a_generic_surface(self):
        _root, window = install_tree(FakeNode("private chat", "label"))
        frame = fake_surface_frame(window, process_kind="wechat")
        with self.assertRaises(driver.ControlFailure) as raised:
            driver.surface_snapshot(frame)
        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)

    def test_surface_close_targets_only_the_twice_verified_bound_x11_window(self):
        content_close = FakeNode(
            "Close", "push button", (430, 300, 140, 45), actions=("click",),
        )
        _root, window = install_tree(content_close)
        frame = fake_surface_frame(window, process_kind="miniprogram")

        with mock.patch.object(driver, "capture_surface_frame", side_effect=(frame, frame)):
            with mock.patch.object(driver, "close_x11_window") as close:
                with mock.patch.object(driver, "x11_wechat_window_inventory", return_value=set()):
                    driver.close_surface({"expected_window_identity": frame["window_identity"]})

        close.assert_called_once_with("0x1234")
        self.assertEqual([], content_close.activated)

    def test_message_surface_cannot_follow_a_preexisting_window(self):
        _root, main_window = install_tree()
        initial = fake_surface_frame(main_window, process_kind="wechat")
        _root, web_window = install_tree()
        opened = fake_surface_frame(web_window, process_kind="web")
        opened["window"]["xid"] = "0x7777"
        opened["window"].pop("digest")
        opened["window"]["digest"] = driver.semantic_digest({
            key: value for key, value in opened["window"].items() if key != "digest"
        })
        opened["window_identity"] = driver.encode_window_identity(opened["window"])
        with mock.patch.object(driver, "capture_surface_frame", return_value=opened):
            with mock.patch.object(driver.time, "monotonic", return_value=0):
                with self.assertRaises(driver.ControlFailure) as raised:
                    driver.wait_for_new_surface(
                        initial["window_identity"],
                        {driver.window_instance(opened["window"])}, "web",
                    )
        self.assertEqual("ACTION_STALE", raised.exception.code)

    def test_message_surface_requires_the_signed_message_node_on_both_frames(self):
        original = FakeNode(
            "Open article", "link", (600, 180, 220, 50),
            description="original", actions=("jump",),
        )
        _root, window = install_tree(original)
        scope = (window, (0,))
        initial = fake_surface_frame(window, process_kind="wechat")
        locator = message_locator(initial, original)
        replacement = FakeNode(
            "Open article", "link", (600, 180, 220, 50),
            description="replacement", actions=("jump",),
        )
        captures = 0

        def capture(*_args):
            nonlocal captures
            captures += 1
            if captures == 1:
                return initial
            window.children[0] = replacement
            return fake_surface_frame(window, process_kind="wechat")

        request = {
            "conversation_title": "Fixture chat",
            "conversation_locator": "conversation-locator",
            "accessible_label": "Open article",
            "surface_locator": locator,
            "kind": "web",
        }
        with mock.patch.object(driver, "select_conversation", return_value=scope):
            with mock.patch.object(driver, "verified_window_identity", return_value=initial["window"]):
                with mock.patch.object(driver, "verify_conversation_header"):
                    with mock.patch.object(driver, "capture_surface_frame", side_effect=capture):
                        with mock.patch.object(driver, "x11_wechat_window_inventory", return_value=set()):
                            with mock.patch.object(driver, "activate_exact_action_candidate") as activate:
                                with self.assertRaises(driver.ControlFailure) as raised:
                                    driver.open_surface(request)

        self.assertEqual("ACTION_STALE", raised.exception.code)
        activate.assert_not_called()
        self.assertEqual([], original.activated)
        self.assertEqual([], replacement.activated)

    def test_message_surface_rejects_an_exact_semantic_clone_with_a_new_atspi_identity(self):
        original = FakeNode(
            "Open article", "link", (600, 180, 220, 50),
            description="same", actions=("jump",),
        )
        _root, window = install_tree(original)
        scope = (window, (0,))
        initial = fake_surface_frame(window, process_kind="wechat")
        locator = message_locator(initial, original)
        replacement = FakeNode(
            "Open article", "link", (600, 180, 220, 50),
            description="same", actions=("jump",),
        )
        self.assertEqual(
            driver.node_signature(original, (0, 0), 0),
            driver.node_signature(replacement, (0, 0), 0),
        )
        self.assertNotEqual(
            driver.observable_accessible_identity(original),
            driver.observable_accessible_identity(replacement),
        )
        captures = 0

        def capture(*_args):
            nonlocal captures
            captures += 1
            if captures == 1:
                return initial
            window.children[0] = replacement
            return fake_surface_frame(window, process_kind="wechat")

        request = {
            "conversation_title": "Fixture chat",
            "conversation_locator": "conversation-locator",
            "accessible_label": "Open article",
            "surface_locator": locator,
            "kind": "web",
        }
        with mock.patch.object(driver, "select_conversation", return_value=scope):
            with mock.patch.object(driver, "verified_window_identity", return_value=initial["window"]):
                with mock.patch.object(driver, "verify_conversation_header"):
                    with mock.patch.object(driver, "capture_surface_frame", side_effect=capture):
                        with mock.patch.object(driver, "x11_wechat_window_inventory", return_value=set()):
                            with mock.patch.object(driver, "wait_for_new_surface") as wait_opened:
                                with self.assertRaises(driver.ControlFailure) as raised:
                                    driver.open_surface(request)

        self.assertEqual("ACTION_STALE", raised.exception.code)
        wait_opened.assert_not_called()
        self.assertEqual([], original.activated)
        self.assertEqual([], replacement.activated)

    def test_message_surface_identity_is_stable_across_retrieved_wrappers(self):
        first = FakeNode(
            "Open article", "link", (600, 180, 220, 50), actions=("jump",),
            accessible_bus=":fixture.99", accessible_path="/fixture/shared/object",
        )
        retrieved_again = FakeNode(
            "Open article", "link", (600, 180, 220, 50), actions=("jump",),
            accessible_bus=":fixture.99", accessible_path="/fixture/shared/object",
        )

        self.assertIsNot(first, retrieved_again)
        self.assertEqual(
            driver.observable_accessible_identity(first),
            driver.observable_accessible_identity(retrieved_again),
        )

    def test_visible_message_surface_is_strictly_bound_or_not_exposed(self):
        card = FakeNode(
            "Open article", "link", (600, 180, 220, 50), actions=("jump",),
        )
        _root, window = install_tree(card)
        frame = fake_surface_frame(window, process_kind="wechat")

        def observe():
            with mock.patch.object(driver, "selected_conversation", return_value={
                "title": "Fixture chat", "locator": "conversation-locator",
            }):
                with mock.patch.object(driver, "active_surface_root", return_value=(window, (0,))):
                    with mock.patch.object(driver, "verified_window_identity", return_value=frame["window"]):
                        with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
                            with mock.patch.object(driver, "verify_conversation_header"):
                                return driver.visible_messages({})

        visible = observe()
        locator = driver.decode_locator(visible["messages"][0]["surface_locator"])
        self.assertEqual(4, locator["version"])
        self.assertTrue(locator["strict_frame"])
        self.assertEqual(frame["window_identity"], locator["window_identity"])
        self.assertEqual(frame["generation"], locator["surface_generation"])
        self.assertEqual(frame["rendered_frame_sha256"], locator["rendered_frame_sha256"])
        self.assertRegex(locator["rendered_region_sha256"], r"^[0-9a-f]{64}$")
        self.assertEqual(
            driver.observable_accessible_identity(card), locator["accessible_identity"],
        )

        card.app.bus_name = ""
        unbound = observe()
        self.assertEqual("", unbound["messages"][0]["surface_kind"])
        self.assertEqual("", unbound["messages"][0]["surface_locator"])

    def test_message_surface_never_treats_delete_as_open(self):
        destructive = FakeNode(
            "Archived card", "list item", (600, 180, 220, 50), actions=("delete",),
        )
        _root, window = install_tree(destructive)

        self.assertIsNone(driver.message_surface_action_index(destructive))
        self.assertEqual("", driver.message_surface_kind(destructive))
        frame = fake_surface_frame(window, process_kind="wechat")
        with mock.patch.object(driver, "selected_conversation", return_value={
            "title": "Fixture chat", "locator": "conversation-locator",
        }):
            with mock.patch.object(driver, "active_surface_root", return_value=(window, (0,))):
                with mock.patch.object(driver, "verified_window_identity", return_value=frame["window"]):
                    with mock.patch.object(driver, "capture_surface_frame", return_value=frame):
                        with mock.patch.object(driver, "verify_conversation_header"):
                            visible = driver.visible_messages({})

        self.assertEqual("", visible["messages"][0]["surface_kind"])
        self.assertEqual("", visible["messages"][0]["surface_locator"])
        self.assertEqual([], destructive.activated)

    def test_message_card_signature_change_is_rejected_without_clicking(self):
        card = FakeNode("Open article", "link", (100, 150, 160, 30), actions=("jump",))
        _root, window = install_tree(card)
        first = driver.unique_exact_action_candidate("Open article", (window, (0,)))
        card.action_names = ["click"]
        second = driver.unique_exact_action_candidate("Open article", (window, (0,)))

        self.assertNotEqual(first, second)
        with self.assertRaises(driver.ControlFailure) as raised:
            driver.activate_exact_action_candidate(first)
        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], card.activated)

    def test_message_exact_candidate_uses_the_same_validated_action_interface(self):
        card = FakeNode("Open article", "link", (100, 150, 160, 30), actions=("click",))
        _root, window = install_tree(card)
        candidate = driver.unique_exact_action_candidate("Open article", (window, (0,)))
        queries = 0
        executed = []

        class SwappingAction(FakeAction):
            def doAction(self, index):
                executed.append(self.names[index])
                return True

        def query_action():
            nonlocal queries
            queries += 1
            return SwappingAction(card, ("click" if queries == 1 else "delete",))

        card.queryAction = query_action
        driver.activate_exact_action_candidate(candidate)

        self.assertEqual(1, queries)
        self.assertEqual(["click"], executed)

    def test_xweb_window_is_not_a_miniprogram(self):
        _root, window = install_tree()
        frame = fake_surface_frame(window, process_kind="web")
        self.assertFalse(driver.miniprogram_surface(frame))

    def test_verified_window_rejects_mismatched_accessibility_owner(self):
        _root, window = install_tree()
        scope = (window, (0,))
        lineage = [{
            "pid": 42, "parent_pid": 1, "starttime": 123456,
            "names": ["wechat"], "executable": "wechat",
        }]
        with mock.patch.object(
            driver, "x11_active_window",
            return_value=("0x1234", 42, ["wechat", "wechat"], (0, 0, 1000, 800)),
        ):
            with mock.patch.object(driver, "process_lineage", return_value=lineage):
                with mock.patch.object(driver, "atspi_owner", return_value=(99, "")):
                    with self.assertRaises(driver.ControlFailure) as raised:
                        driver.verified_window_identity(scope)
        self.assertEqual("ACTION_STALE", raised.exception.code)

    def test_overlapping_window_geometry_is_not_enough_to_bind_windows(self):
        self.assertTrue(driver.matching_window_geometry((10, 10, 400, 500), (12, 14, 390, 480)))
        self.assertFalse(driver.matching_window_geometry((10, 10, 400, 500), (100, 100, 220, 300)))

    def test_screenshot_dimensions_must_match_bound_x11_window(self):
        identity = {"geometry": [10, 20, 400, 500]}
        driver.require_screenshot_geometry(identity, (400, 500))
        with self.assertRaises(driver.ControlFailure) as raised:
            driver.require_screenshot_geometry(identity, (800, 1000))
        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)

    def test_surface_name_normalization_preserves_word_boundaries_and_rejects_formatting(self):
        self.assertEqual("Campus Cat", driver.validated_surface_name("  Campus   Cat  "))
        self.assertNotEqual(
            driver.normalized_exact("Campus Cat"),
            driver.normalized_exact("CampusCat"),
        )
        with self.assertRaises(driver.ControlFailure):
            driver.validated_surface_name("Campus\u202eCat")


if __name__ == "__main__":
    unittest.main()
