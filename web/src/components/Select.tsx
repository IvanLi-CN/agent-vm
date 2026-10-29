import * as SelectPrimitive from "@radix-ui/react-select";
import { Check, ChevronDown, ChevronUp } from "lucide-react";
import { forwardRef, type ComponentPropsWithoutRef, type ElementRef } from "react";

export const Select = SelectPrimitive.Root;
export const SelectGroup = SelectPrimitive.Group;
export const SelectValue = SelectPrimitive.Value;

export const SelectTrigger = forwardRef<
  ElementRef<typeof SelectPrimitive.Trigger>,
  ComponentPropsWithoutRef<typeof SelectPrimitive.Trigger>
>(function SelectTrigger({ className = "", children, ...props }, ref) {
  return <SelectPrimitive.Trigger ref={ref} className={`select-trigger ${className}`} {...props}>
    {children}
    <SelectPrimitive.Icon className="select-trigger-icon" asChild>
      <ChevronDown aria-hidden="true" />
    </SelectPrimitive.Icon>
  </SelectPrimitive.Trigger>;
});

export const SelectContent = forwardRef<
  ElementRef<typeof SelectPrimitive.Content>,
  ComponentPropsWithoutRef<typeof SelectPrimitive.Content>
>(function SelectContent({ className = "", children, position = "popper", sideOffset = 4, ...props }, ref) {
  return <SelectPrimitive.Portal>
    <SelectPrimitive.Content ref={ref} className={`select-content ${className}`} position={position} sideOffset={sideOffset} {...props}>
      <SelectPrimitive.ScrollUpButton className="select-scroll-button">
        <ChevronUp aria-hidden="true" />
      </SelectPrimitive.ScrollUpButton>
      <SelectPrimitive.Viewport className="select-viewport">{children}</SelectPrimitive.Viewport>
      <SelectPrimitive.ScrollDownButton className="select-scroll-button">
        <ChevronDown aria-hidden="true" />
      </SelectPrimitive.ScrollDownButton>
    </SelectPrimitive.Content>
  </SelectPrimitive.Portal>;
});

export const SelectItem = forwardRef<
  ElementRef<typeof SelectPrimitive.Item>,
  ComponentPropsWithoutRef<typeof SelectPrimitive.Item>
>(function SelectItem({ className = "", children, ...props }, ref) {
  return <SelectPrimitive.Item ref={ref} className={`select-item ${className}`} {...props}>
    <SelectPrimitive.ItemText>{children}</SelectPrimitive.ItemText>
    <SelectPrimitive.ItemIndicator className="select-item-indicator">
      <Check aria-hidden="true" />
    </SelectPrimitive.ItemIndicator>
  </SelectPrimitive.Item>;
});

export const SelectLabel = forwardRef<
  ElementRef<typeof SelectPrimitive.Label>,
  ComponentPropsWithoutRef<typeof SelectPrimitive.Label>
>(function SelectLabel({ className = "", ...props }, ref) {
  return <SelectPrimitive.Label ref={ref} className={`select-label ${className}`} {...props} />;
});

export const SelectSeparator = forwardRef<
  ElementRef<typeof SelectPrimitive.Separator>,
  ComponentPropsWithoutRef<typeof SelectPrimitive.Separator>
>(function SelectSeparator({ className = "", ...props }, ref) {
  return <SelectPrimitive.Separator ref={ref} className={`select-separator ${className}`} {...props} />;
});
