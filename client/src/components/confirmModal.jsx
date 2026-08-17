// material-ui
import { LoadingButton } from '@mui/lab';
import { Button } from '@mui/material';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogContentText from '@mui/material/DialogContentText';
import DialogTitle from '@mui/material/DialogTitle';
import * as React from 'react';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';

// variant/color were already being passed by call sites but ignored; they now
// apply, with the previous hardcoded values as the defaults.
const ConfirmModal = ({ callback, label, content, startIcon, variant = 'outlined', color = 'warning' }) => {
    const { t } = useTranslation();
    const [openModal, setOpenModal] = useState(false);

    return <>
      <Dialog open={openModal} onClose={() => setOpenModal(false)}>
          <DialogTitle>Are you sure?</DialogTitle>
          <DialogContent>
              <DialogContentText>
                  {content}
              </DialogContentText>
          </DialogContent>
          <DialogActions>
              <Button onClick={() => {
                  setOpenModal(false);           
              }}>{t('global.cancelAction')}</Button>
              <LoadingButton
              onClick={() => {   
                  callback();     
                  setOpenModal(false);    
              }}>{t('global.confirmAction')}</LoadingButton>
          </DialogActions>
      </Dialog>

      <Button
          disableElevation
          variant={variant}
          color={color}
          startIcon={startIcon}
          onClick={() => {
              setOpenModal(true);
          }}
      >
        {label}
      </Button>
    </>
};


const ConfirmModalDirect = ({ callback, content, onClose }) => {
    const [openModal, setOpenModal] = useState(true);
    const { t } = useTranslation();

    return <>
      <Dialog open={openModal} onClose={() => {
        onClose && onClose();
        setOpenModal(false);
      }}>
          <DialogTitle>{t('global.confirmDeletion')}</DialogTitle>
          <DialogContent>
              <DialogContentText>
                  {content}
              </DialogContentText>
          </DialogContent>
          <DialogActions>
              <Button onClick={() => {
                  setOpenModal(false);    
                  onClose && onClose();
              }}>{t('global.cancelAction')}</Button>
              <LoadingButton
              onClick={() => {   
                  callback();     
                  setOpenModal(false);    
                  onClose && onClose();
              }}>{t('global.confirmAction')}</LoadingButton>
          </DialogActions>
      </Dialog>
    </>
};

export default ConfirmModal;
export { ConfirmModalDirect };
