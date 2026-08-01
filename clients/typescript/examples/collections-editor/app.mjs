import { EditorialCollectionsModel } from "./editor-model.mjs";

const model = new EditorialCollectionsModel(`editor-${crypto.randomUUID()}`);
const title = requiredElement("title");
const label = requiredElement("label");
const section = requiredElement("section");
const labels = requiredElement("labels");
const outline = requiredElement("outline");
const revisions = requiredElement("revisions");
const error = requiredElement("error");

requiredElement("title-form").addEventListener("submit", (event) => {
  event.preventDefault();
  run(() => model.setTitle(title.value));
});
requiredElement("label-form").addEventListener("submit", (event) => {
  event.preventDefault();
  run(() => {
    model.addLabel(label.value);
    label.value = "";
  });
});
requiredElement("section-form").addEventListener("submit", (event) => {
  event.preventDefault();
  run(() => {
    model.addSection(section.value);
    section.value = "";
  });
});

model.observe(render);
render();

function render() {
  const state = model.state();
  if (document.activeElement !== title) title.value = state.title;
  revisions.textContent = state.revisions.toString();
  labels.replaceChildren(...state.labels.map((value) => {
    const item = document.createElement("li");
    const remove = document.createElement("button");
    remove.type = "button";
    remove.textContent = `Remove ${value}`;
    remove.addEventListener("click", () => run(() => model.removeLabel(value)));
    item.append(value, " ", remove);
    return item;
  }));
  outline.replaceChildren(...state.outline.map(renderNode));
}

function renderNode(node) {
  const item = document.createElement("li");
  item.textContent = node.value.kind === "section" ? node.value.title : node.value.text;
  if (node.children.length > 0) {
    const children = document.createElement("ul");
    children.append(...node.children.map(renderNode));
    item.append(children);
  }
  return item;
}

function run(operation) {
  try {
    error.textContent = "";
    operation();
  } catch (cause) {
    error.textContent = cause instanceof Error ? cause.message : String(cause);
  }
}

function requiredElement(id) {
  const element = document.getElementById(id);
  if (element === null) throw new Error(`missing #${id}`);
  return element;
}
