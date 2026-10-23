// 仅控制手机端的折叠面板和输入框高度，不改变终端指令及权限判断。
export function mountCmuxCompact({
  media, statusButton, mobileTools, extraKeys, attachmentToggle, terminalPane,
  prompt, statusElement, filesButton, browserButton, refreshButton, statusDurationMs = 4500
}) {
  let statusTimer;
  const showStatus = (message, reveal = true) => {
    statusElement.textContent = message;
    statusButton.setAttribute("aria-label", message);
    clearTimeout(statusTimer);
    if (media.matches && reveal) {
      statusButton.classList.add("expanded");
      statusTimer = setTimeout(() => statusButton.classList.remove("expanded"), statusDurationMs);
    } else {
      statusButton.classList.remove("expanded");
    }
  };
  const syncPromptHeight = () => {
    if (!media.matches) {
      prompt.style.height = "";
      return;
    }
    prompt.style.height = "44px";
    prompt.style.height = `${Math.min(112, Math.max(44, prompt.scrollHeight))}px`;
  };
  const setAttachmentOpen = open => {
    terminalPane.classList.toggle("attachments-open", open);
    attachmentToggle.setAttribute("aria-expanded", String(open));
  };
  statusButton.addEventListener("click", () => {
    if (media.matches) {
      clearTimeout(statusTimer);
      statusButton.classList.toggle("expanded");
    }
  });
  attachmentToggle.addEventListener("click", () => {
    setAttachmentOpen(!terminalPane.classList.contains("attachments-open"));
  });
  for (const [button, original] of [
    [document.getElementById("mobileFiles"), filesButton],
    [document.getElementById("mobileBrowser"), browserButton],
    [document.getElementById("mobileRefresh"), refreshButton]
  ]) {
    button.addEventListener("click", () => {
      mobileTools.open = false;
      original.click();
    });
  }
  extraKeys.querySelectorAll("[data-key]").forEach(button =>
    button.addEventListener("click", () => { extraKeys.open = false; }));
  document.addEventListener("pointerdown", event => {
    if (!statusButton.contains(event.target)) statusButton.classList.remove("expanded");
    if (!mobileTools.contains(event.target)) mobileTools.open = false;
    if (!extraKeys.contains(event.target)) extraKeys.open = false;
  });
  document.addEventListener("keydown", event => {
    if (event.key !== "Escape") return;
    statusButton.classList.remove("expanded");
    mobileTools.open = false;
    extraKeys.open = false;
  });
  media.addEventListener("change", () => {
    clearTimeout(statusTimer);
    statusButton.classList.remove("expanded");
    mobileTools.open = false;
    extraKeys.open = false;
    syncPromptHeight();
  });
  syncPromptHeight();
  return { syncPromptHeight, setAttachmentOpen, showStatus };
}
