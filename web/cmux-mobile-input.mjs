// 一些手机输入法只向 xterm 的隐藏 textarea 派发 insertText，不产生可用的 keydown。
// 在 xterm 的监听器之前接收已确认的普通文本；输入法组合输入仍交给 xterm 处理。
export function bindMobileTerminalTextInput({ textarea, isActive, sendText }) {
  const target = textarea.parentElement || textarea;
  const capture = { capture: true };
  let previousValue = textarea.value;
  let composing = false;
  let endingComposition = false;
  let compositionTimer;

  const committedText = event => isActive() && !composing && !event.isComposing &&
    !endingComposition && event.inputType === "insertText" &&
    typeof event.data === "string" && event.data.length > 0;
  const rememberValue = () => { previousValue = textarea.value; };
  const onKeyDown = event => { if (event.target === textarea) rememberValue(); };
  const onBeforeInput = event => {
    if (event.target !== textarea || !committedText(event) || !event.cancelable) return;
    rememberValue();
    event.preventDefault();
    event.stopImmediatePropagation();
    sendText(event.data);
  };
  const onInput = event => {
    if (event.target !== textarea || !isActive() || event.inputType !== "insertText") return;
    if (composing || event.isComposing || endingComposition) {
      event.stopImmediatePropagation();
      return;
    }
    if (!committedText(event)) return;
    event.stopImmediatePropagation();
    textarea.value = previousValue;
    sendText(event.data);
  };
  const onCompositionStart = () => {
    clearTimeout(compositionTimer);
    composing = true;
    endingComposition = false;
  };
  const onCompositionEnd = () => {
    composing = false;
    endingComposition = true;
    clearTimeout(compositionTimer);
    compositionTimer = setTimeout(() => {
      endingComposition = false;
      rememberValue();
    }, 0);
  };

  target.addEventListener("keydown", onKeyDown, capture);
  target.addEventListener("beforeinput", onBeforeInput, capture);
  target.addEventListener("input", onInput, capture);
  textarea.addEventListener("compositionstart", onCompositionStart);
  textarea.addEventListener("compositionend", onCompositionEnd);
  return () => {
    clearTimeout(compositionTimer);
    target.removeEventListener("keydown", onKeyDown, capture);
    target.removeEventListener("beforeinput", onBeforeInput, capture);
    target.removeEventListener("input", onInput, capture);
    textarea.removeEventListener("compositionstart", onCompositionStart);
    textarea.removeEventListener("compositionend", onCompositionEnd);
  };
}
