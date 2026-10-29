import * as SelectPrimitive from "@radix-ui/react-select";
import { Check, ChevronDown, ChevronUp } from "lucide-react";
import { forwardRef, type ComponentPropsWithoutRef, type ElementRef } from "react";
import { cn } from "@/lib/utils";

const Select = SelectPrimitive.Root;
const SelectGroup = SelectPrimitive.Group;
const SelectValue = SelectPrimitive.Value;

const SelectTrigger = forwardRef<ElementRef<typeof SelectPrimitive.Trigger>, ComponentPropsWithoutRef<typeof SelectPrimitive.Trigger>>(function SelectTrigger({ className, children, ...props }, ref) {
  return <SelectPrimitive.Trigger ref={ref} data-slot="select-trigger" className={cn("flex h-9 w-full items-center justify-between gap-2 rounded-[var(--radius-control)] border border-[var(--control)] bg-[var(--surface)] px-3 py-2 text-left text-sm text-[var(--text)] outline-none transition-colors hover:bg-[var(--raised)] focus-visible:ring-2 focus-visible:ring-[var(--accent)] focus-visible:ring-offset-2 focus-visible:ring-offset-[var(--surface)] data-[state=open]:border-[var(--accent)] data-[state=open]:shadow-[0_0_0_2px_var(--accent-soft)] disabled:cursor-not-allowed disabled:opacity-50 [&>span]:line-clamp-1 [&>svg]:size-4 [&>svg]:shrink-0 [&>svg]:text-[var(--muted)]", className)} {...props}>
    {children}
    <SelectPrimitive.Icon asChild><ChevronDown aria-hidden="true" /></SelectPrimitive.Icon>
  </SelectPrimitive.Trigger>;
});

const SelectContent = forwardRef<ElementRef<typeof SelectPrimitive.Content>, ComponentPropsWithoutRef<typeof SelectPrimitive.Content>>(function SelectContent({ className, children, position = "popper", sideOffset = 4, ...props }, ref) {
  return <SelectPrimitive.Portal><SelectPrimitive.Content ref={ref} data-slot="select-content" position={position} sideOffset={sideOffset} className={cn("relative z-50 max-h-[var(--radix-select-content-available-height)] min-w-[var(--radix-select-trigger-width)] overflow-hidden rounded-[var(--radius-control)] border border-[var(--control)] bg-[var(--surface)] p-1 text-[var(--text)] shadow-[0_14px_30px_rgba(19,20,22,.16)] data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0", className)} {...props}>
    <SelectPrimitive.ScrollUpButton className="flex h-5 items-center justify-center text-[var(--muted)]"><ChevronUp aria-hidden="true" /></SelectPrimitive.ScrollUpButton>
    <SelectPrimitive.Viewport className="p-0.5">{children}</SelectPrimitive.Viewport>
    <SelectPrimitive.ScrollDownButton className="flex h-5 items-center justify-center text-[var(--muted)]"><ChevronDown aria-hidden="true" /></SelectPrimitive.ScrollDownButton>
  </SelectPrimitive.Content></SelectPrimitive.Portal>;
});

const SelectItem = forwardRef<ElementRef<typeof SelectPrimitive.Item>, ComponentPropsWithoutRef<typeof SelectPrimitive.Item>>(function SelectItem({ className, children, ...props }, ref) {
  return <SelectPrimitive.Item ref={ref} data-slot="select-item" className={cn("relative flex min-h-8 cursor-pointer select-none items-center rounded px-2 py-1.5 pr-8 text-sm outline-none data-[highlighted]:bg-[var(--active)] data-[state=checked]:bg-[var(--accent-soft)] data-[disabled]:pointer-events-none data-[disabled]:opacity-50", className)} {...props}>
    <SelectPrimitive.ItemText>{children}</SelectPrimitive.ItemText>
    <span className="absolute right-2 flex size-4 items-center justify-center"><SelectPrimitive.ItemIndicator><Check className="size-4 text-[var(--accent)]" aria-hidden="true" /></SelectPrimitive.ItemIndicator></span>
  </SelectPrimitive.Item>;
});

const SelectLabel = forwardRef<ElementRef<typeof SelectPrimitive.Label>, ComponentPropsWithoutRef<typeof SelectPrimitive.Label>>(function SelectLabel({ className, ...props }, ref) {
  return <SelectPrimitive.Label ref={ref} data-slot="select-label" className={cn("px-2 py-1.5 text-xs text-[var(--muted)]", className)} {...props} />;
});

const SelectSeparator = forwardRef<ElementRef<typeof SelectPrimitive.Separator>, ComponentPropsWithoutRef<typeof SelectPrimitive.Separator>>(function SelectSeparator({ className, ...props }, ref) {
  return <SelectPrimitive.Separator ref={ref} data-slot="select-separator" className={cn("-mx-1 my-1 h-px bg-[var(--rule)]", className)} {...props} />;
});

export { Select, SelectGroup, SelectValue, SelectTrigger, SelectContent, SelectItem, SelectLabel, SelectSeparator };
