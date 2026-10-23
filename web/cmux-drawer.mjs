// 手机工作区使用抽屉，避免侧栏把真实终端挤成很小的可视区域。
export function mountCmuxDrawer({ button, aside, backdrop, body, media }) {
  let open = false;
  const render = () => {
    const visible = open && media.matches;
    body.classList.toggle("cmux-drawer-open", visible);
    button.setAttribute("aria-expanded", String(visible));
    aside.setAttribute("aria-hidden", String(media.matches && !visible));
    backdrop.hidden = !visible;
  };
  const toggle = () => { open = !open; render(); };
  const close = () => { open = false; render(); };
  button.addEventListener("click", toggle);
  backdrop.addEventListener("click", close);
  media.addEventListener("change", close);
  render();
  return { close };
}
