#!/usr/bin/env python3
"""Unit tests for fail-closed WeChat accessibility target selection."""

import importlib.util
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
    def __init__(
        self, name="", role="label", geometry=(0, 0, 20, 20), *,
        description="", states=("showing", "visible"), actions=(), editable=False, readable=True,
        children=(), text="",
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

    @property
    def childCount(self):
        return len(self.children)

    def getChildAtIndex(self, index):
        return self.children[index]

    def getRoleName(self):
        return self.role

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

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.submit_auth_code("123456")

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)
        self.assertEqual("", editor.text)

    def test_message_body_mismatch_fails_closed(self):
        editor = FakeNode("", "entry", editable=True, text="different")

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.verify_text_contents(editor, "expected", timeout=0)

        self.assertEqual("CLIENT_INCOMPATIBLE", raised.exception.code)

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
        scope = (window, (0,))
        nodes = driver.scope_nodes(scope)
        generation = driver.surface_generation(window, (0,), nodes)
        encoded = driver.make_locator(
            (0, 0), "share", 0, node=share,
            surface_generation_value=generation,
        )
        locator = driver.decode_locator(encoded)
        validated, _scope = driver.validate_surface_locator(
            locator, expected_kind="share", require_unique_kind=True,
        )
        self.assertEqual("Share", validated.name)

        share.name = "Pay"
        with self.assertRaises(driver.ControlFailure) as raised:
            driver.validate_surface_locator(
                locator, expected_kind="share", require_unique_kind=True,
            )
        self.assertEqual("ACTION_STALE", raised.exception.code)
        self.assertEqual([], share.activated)

    def test_duplicate_share_actions_are_ambiguous(self):
        first = FakeNode("Share", "push button", (700, 100, 80, 30), actions=("click",))
        second = FakeNode("Share", "push button", (800, 100, 80, 30), actions=("click",))
        _root, window = install_tree(first, second)
        scope = (window, (0,))
        nodes = driver.scope_nodes(scope)
        generation = driver.surface_generation(window, (0,), nodes)
        locator = driver.decode_locator(driver.make_locator(
            (0, 0), "share", 0, node=first,
            surface_generation_value=generation,
        ))

        with self.assertRaises(driver.ControlFailure) as raised:
            driver.validate_surface_locator(
                locator, expected_kind="share", require_unique_kind=True,
            )

        self.assertEqual("TARGET_AMBIGUOUS", raised.exception.code)
        self.assertEqual([], first.activated)
        self.assertEqual([], second.activated)


if __name__ == "__main__":
    unittest.main()
