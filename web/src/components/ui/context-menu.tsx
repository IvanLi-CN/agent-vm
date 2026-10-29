import * as ContextMenuPrimitive from "@radix-ui/react-context-menu";
import { Check } from "lucide-react";
import { type ComponentPropsWithoutRef, type ElementRef, forwardRef } from "react";
import { cn } from "@/lib/utils";

const ContextMenu = ContextMenuPrimitive.Root;
const ContextMenuTrigger = ContextMenuPrimitive.Trigger;
const ContextMenuRadioGroup = ContextMenuPrimitive.RadioGroup;

const ContextMenuContent = forwardRef<ElementRef<typeof ContextMenuPrimitive.Content>, ComponentPropsWithoutRef<typeof ContextMenuPrimitive.Content>>(function ContextMenuContent({ className, ...props }, ref) {
  return <ContextMenuPrimitive.Portal><ContextMenuPrimitive.Content ref={ref} data-slot="context-menu-content" className={cn("z-50 min-w-44 overflow-hidden rounded-[var(--radius-control)] border border-[var(--border)] bg-[var(--surface)] p-1 text-[var(--text)] shadow-[0_12px_28px_rgba(19,20,22,.16)]", className)} {...props} /></ContextMenuPrimitive.Portal>;
});

const ContextMenuRadioItem = forwardRef<ElementRef<typeof ContextMenuPrimitive.RadioItem>, ComponentPropsWithoutRef<typeof ContextMenuPrimitive.RadioItem>>(function ContextMenuRadioItem({ className, children, ...props }, ref) {
  return <ContextMenuPrimitive.RadioItem ref={ref} data-slot="context-menu-radio-item" className={cn("relative flex min-h-9 cursor-default select-none items-center gap-2 rounded px-2 py-1.5 pr-8 text-sm outline-none focus:bg-[var(--active)] data-[disabled]:pointer-events-none data-[disabled]:opacity-50", className)} {...props}>
    {children}
    <span className="pointer-events-none absolute right-2 flex size-4 items-center justify-center"><ContextMenuPrimitive.ItemIndicator><Check className="size-4 text-[var(--accent)]" aria-hidden="true" /></ContextMenuPrimitive.ItemIndicator></span>
  </ContextMenuPrimitive.RadioItem>;
});

const ContextMenuSeparator = forwardRef<ElementRef<typeof ContextMenuPrimitive.Separator>, ComponentPropsWithoutRef<typeof ContextMenuPrimitive.Separator>>(function ContextMenuSeparator({ className, ...props }, ref) {
  return <ContextMenuPrimitive.Separator ref={ref} data-slot="context-menu-separator" className={cn("-mx-1 my-1 h-px bg-[var(--rule)]", className)} {...props} />;
});

export { ContextMenu, ContextMenuTrigger, ContextMenuContent, ContextMenuRadioGroup, ContextMenuRadioItem, ContextMenuSeparator };
