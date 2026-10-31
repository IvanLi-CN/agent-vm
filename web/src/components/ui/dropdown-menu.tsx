import * as DropdownMenuPrimitive from "@radix-ui/react-dropdown-menu";
import { Check, ChevronRight } from "lucide-react";
import { type ComponentPropsWithoutRef, type ElementRef, forwardRef } from "react";
import { cn } from "@/lib/utils";

const DropdownMenu = DropdownMenuPrimitive.Root;
const DropdownMenuTrigger = DropdownMenuPrimitive.Trigger;

const DropdownMenuContent = forwardRef<ElementRef<typeof DropdownMenuPrimitive.Content>, ComponentPropsWithoutRef<typeof DropdownMenuPrimitive.Content>>(function DropdownMenuContent({ className, sideOffset = 4, align = "end", ...props }, ref) {
  return <DropdownMenuPrimitive.Portal><DropdownMenuPrimitive.Content ref={ref} data-slot="dropdown-menu-content" sideOffset={sideOffset} align={align} className={cn("z-50 min-w-44 overflow-hidden rounded-[var(--radius-control)] border border-[var(--border)] bg-[var(--surface)] p-1 text-[var(--text)] shadow-[0_12px_28px_rgba(19,20,22,.16)] data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0", className)} {...props} /></DropdownMenuPrimitive.Portal>;
});

const DropdownMenuGroup = DropdownMenuPrimitive.Group;

const DropdownMenuRadioGroup = DropdownMenuPrimitive.RadioGroup;

const DropdownMenuRadioItem = forwardRef<ElementRef<typeof DropdownMenuPrimitive.RadioItem>, ComponentPropsWithoutRef<typeof DropdownMenuPrimitive.RadioItem>>(function DropdownMenuRadioItem({ className, children, ...props }, ref) {
  return <DropdownMenuPrimitive.RadioItem ref={ref} data-slot="dropdown-menu-radio-item" className={cn("relative flex min-h-9 cursor-default select-none items-center gap-2 rounded px-2 py-1.5 pr-8 text-sm outline-none focus:bg-[var(--active)] data-[disabled]:pointer-events-none data-[disabled]:opacity-50", className)} {...props}>
    {children}
    <span className="pointer-events-none absolute right-2 flex size-4 items-center justify-center"><DropdownMenuPrimitive.ItemIndicator><Check className="size-4 text-[var(--accent)]" aria-hidden="true" /></DropdownMenuPrimitive.ItemIndicator></span>
  </DropdownMenuPrimitive.RadioItem>;
});

const DropdownMenuSeparator = forwardRef<ElementRef<typeof DropdownMenuPrimitive.Separator>, ComponentPropsWithoutRef<typeof DropdownMenuPrimitive.Separator>>(function DropdownMenuSeparator({ className, ...props }, ref) {
  return <DropdownMenuPrimitive.Separator ref={ref} data-slot="dropdown-menu-separator" className={cn("-mx-1 my-1 h-px bg-[var(--rule)]", className)} {...props} />;
});

const DropdownMenuItem = forwardRef<ElementRef<typeof DropdownMenuPrimitive.Item>, ComponentPropsWithoutRef<typeof DropdownMenuPrimitive.Item>>(function DropdownMenuItem({ className, ...props }, ref) {
  return <DropdownMenuPrimitive.Item ref={ref} data-slot="dropdown-menu-item" className={cn("relative flex min-h-9 cursor-default select-none items-center gap-2 rounded px-2 py-1.5 text-sm outline-none focus:bg-[var(--active)] data-[disabled]:pointer-events-none data-[disabled]:opacity-50 [&_svg]:size-4", className)} {...props} />;
});

export { DropdownMenu, DropdownMenuTrigger, DropdownMenuContent, DropdownMenuGroup, DropdownMenuItem, DropdownMenuRadioGroup, DropdownMenuRadioItem, DropdownMenuSeparator, ChevronRight };
