import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { Editor } from "@tiptap/core";

import type { CodeMirrorTextPort, MonacoTextPort, TiptapRichTextPort, TiptapTextPort } from "../src/bindings.js";
import type { YjsCodeMirrorTextPort } from "../src/yjs.js";

declare const codeMirrorView: EditorView;
declare const tiptapEditor: Editor;
declare const monacoModel: {
  getValue(): string;
  getValueLength(): number;
  setValue(value: string): void;
  onDidChangeContent(listener: (event: {
    readonly changes: readonly {
      readonly rangeOffset: number;
      readonly rangeLength: number;
      readonly text: string;
    }[];
    readonly isFlush: boolean;
    readonly isEolChange: boolean;
  }) => void): { dispose(): void };
};

const codeMirrorPort: CodeMirrorTextPort = codeMirrorView;
const monacoPort: MonacoTextPort = monacoModel;
const yjsCodeMirrorPort: YjsCodeMirrorTextPort = codeMirrorView;
const tiptapPort: TiptapTextPort = tiptapEditor;
const tiptapRichTextPort: TiptapRichTextPort = tiptapEditor;

void codeMirrorPort;
void monacoPort;
void yjsCodeMirrorPort;
void tiptapPort;
void tiptapRichTextPort;
void EditorState;
