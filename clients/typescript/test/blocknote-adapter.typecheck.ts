import { BlockNoteEditor } from "@blocknote/core";

import { bindBlockNoteRichText } from "../src/bindings.js";
import type { BlockNoteRichTextPort, RichTextEditorDocument } from "../src/bindings.js";

declare const document: RichTextEditorDocument;

const editor = BlockNoteEditor.create();
const port: BlockNoteRichTextPort = editor;
bindBlockNoteRichText(document, editor, { onLocalFrame() {} });

void port;
