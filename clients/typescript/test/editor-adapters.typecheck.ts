import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { Editor } from "@tiptap/core";

import type { CodeMirrorTextPort, TiptapTextPort } from "../src/bindings.js";
import type { YjsCodeMirrorTextPort } from "../src/yjs.js";

declare const codeMirrorView: EditorView;
declare const tiptapEditor: Editor;

const codeMirrorPort: CodeMirrorTextPort = codeMirrorView;
const yjsCodeMirrorPort: YjsCodeMirrorTextPort = codeMirrorView;
const tiptapPort: TiptapTextPort = tiptapEditor;

void codeMirrorPort;
void yjsCodeMirrorPort;
void tiptapPort;
void EditorState;
