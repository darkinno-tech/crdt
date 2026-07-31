import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { Editor } from "@tiptap/core";

import type { CodeMirrorTextPort, TiptapTextPort } from "../src/bindings.js";

declare const codeMirrorView: EditorView;
declare const tiptapEditor: Editor;

const codeMirrorPort: CodeMirrorTextPort = codeMirrorView;
const tiptapPort: TiptapTextPort = tiptapEditor;

void codeMirrorPort;
void tiptapPort;
void EditorState;
